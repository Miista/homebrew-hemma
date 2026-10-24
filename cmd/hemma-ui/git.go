package main

import (
	"context"
	"encoding/json"
	"hemma/internal/cli/kernel"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Change tracking: edits write straight to the repo (hemma's sync), and the
// repo's git working tree IS the list of pending changes. apply/deploy are
// refused while it is dirty, so every change that goes live carries a commit
// message the operator wrote. If the repo is not a git repository the gate is
// off (nothing to commit to) and /api/changes says so.

type change struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

type changes struct {
	Git    bool     `json:"git"`
	Files  []change `json:"files"`
	Branch string   `json:"branch,omitempty"`
	// HasRemote/Ahead: deploy pulls from origin and refuses unpushed commits.
	HasRemote bool `json:"has_remote"`
	Ahead     int  `json:"ahead"`
	// AheadHemma: some of those unpushed commits change a file hemma owns, so there is something to deploy. Commits to
	// anything else in the repo (a compose file, say) are ahead of origin too, but hemma has nothing to make live for them.
	AheadHemma bool `json:"ahead_hemma"`
}

func (s *server) git(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// safe.directory: the bind-mounted repo is owned by the host user, not the container's.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", s.repo, "-c", "safe.directory=*"}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+env("HEMMA_UI_GIT_NAME", "hemma-ui"),
		"GIT_AUTHOR_EMAIL="+env("HEMMA_UI_GIT_EMAIL", "hemma-ui@localhost"),
		"GIT_COMMITTER_NAME="+env("HEMMA_UI_GIT_NAME", "hemma-ui"),
		"GIT_COMMITTER_EMAIL="+env("HEMMA_UI_GIT_EMAIL", "hemma-ui@localhost"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// changesCached is changes() for the read-only endpoints: a page load asks /api/changes and /api/diff together, and
// each needs the same `git status`, which on a big repo (data directories) is the slowest thing the server does.
// Concurrent callers share one run (the lock is held while it runs), and a result is reused for a couple of
// seconds. Anything that changes the repo calls invalidateChanges when it is done, and anything that DECIDES on
// the working tree (the apply/deploy gate, commit, discard) calls changes() for a fresh answer.
func (s *server) changesCached() changes {
	s.cc.mu.Lock()
	defer s.cc.mu.Unlock()
	if !s.cc.at.IsZero() && time.Since(s.cc.at) < 2*time.Second {
		return s.cc.val
	}
	s.cc.val = s.changes()
	s.cc.at = time.Now()
	return s.cc.val
}

func (s *server) invalidateChanges() {
	s.cc.mu.Lock()
	s.cc.at = time.Time{}
	s.cc.mu.Unlock()
}

type changesCache struct {
	mu  sync.Mutex
	at  time.Time
	val changes
}

func (s *server) changes() changes {
	c := changes{Files: []change{}}
	if out, err := s.git(10*time.Second, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return c
	}
	c.Git = true
	out, _ := s.git(20*time.Second, "status", "--porcelain=v1", "--untracked-files=all")
	// Only what hemma is responsible for counts as a change here (kernel.OwnedPaths: services.yaml, the manifest and
	// every file the manifest lists). The rest of the repo is somebody else's work: it is not shown, not committed by
	// Save, and does not hold back Deploy.
	owned := kernel.OwnedPaths(s.repo, func(p string) ([]byte, bool) {
		b, err := s.git(10*time.Second, "show", "HEAD:"+p)
		return []byte(b), err == nil
	})
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(l) > 3 && owned[unquoteRename(l[3:])] {
			c.Files = append(c.Files, change{Status: strings.TrimSpace(l[:2]), Path: l[3:]})
		}
	}
	if b, err := s.git(5*time.Second, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		c.Branch = strings.TrimSpace(b)
	}
	if n, err := s.git(5*time.Second, "rev-list", "--count", "@{u}..HEAD"); err == nil {
		c.HasRemote = true
		c.Ahead, _ = strconv.Atoi(strings.TrimSpace(n))
		if c.Ahead > 0 {
			// --no-renames: a rename touches both its old and its new path
			if names, err := s.git(20*time.Second, "diff", "--name-only", "--no-renames", "-z", "@{u}..HEAD"); err == nil {
				for _, f := range strings.Split(names, "\x00") {
					if owned[f] {
						c.AheadHemma = true
						break
					}
				}
			}
		}
	}
	return c
}

// unquoteRename turns a porcelain path into the path to look up: a rename "old -> new" is its new path.
func unquoteRename(p string) string {
	if i := strings.LastIndex(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return p
}

// There is no push here: sending commits to origin is the first step of `hemma deploy` itself (on this, the deploy
// host: pull --rebase when origin moved, then push), so Save only commits.

func (s *server) handleChanges(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.changesCached())
}

// handleCommit commits every pending change under the operator's message: hemma's own files (changes() lists only
// those), named explicitly, so nothing else in the repo — somebody's half-done edit to a compose file — is staged or
// committed along with them.
func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "invalid request body"})
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" || len(msg) > 2000 {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "a change message is required (max 2000 characters)"})
		return
	}
	// Line breaks (LF or CRLF) and tabs are fine in a message; other control characters are not text and are refused.
	if strings.ContainsFunc(msg, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f }) {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "the change message contains control characters"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidateChanges()
	ch := s.changes()
	if !ch.Git {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "the repo is not a git repository"})
		return
	}
	if len(ch.Files) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "nothing to commit"})
		return
	}
	var sb strings.Builder
	paths := make([]string, 0, len(ch.Files))
	for _, f := range ch.Files {
		paths = append(paths, unquoteRename(f.Path))
	}
	// Two git commands (a new generated file is untracked until it is added, and commit takes only known paths), made
	// all-or-nothing: if the commit fails after the add (a hook, a lock file), the add is undone for just these
	// paths, so nothing is left staged — the working tree is untouched either way, and Discard still knows every file.
	out, err := s.git(time.Minute, append([]string{"add", "-A", "--"}, paths...)...)
	sb.WriteString(out)
	if err == nil {
		out, err = s.git(time.Minute, append([]string{"commit", "--message=" + msg, "--"}, paths...)...) // --only: just these, whatever else is staged
		sb.WriteString(out)
		if err != nil {
			if undo, uerr := s.git(time.Minute, append([]string{"reset", "-q", "--"}, paths...)...); uerr != nil {
				sb.WriteString("\n(could not unstage after the failed commit: " + undo + ")")
			}
		}
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "output": sb.String()})
		return
	}
	log.Printf("commit: %d files", len(ch.Files))
	writeJSON(w, 200, map[string]any{"ok": true, "output": sb.String()})
}

// gated reports whether a command may not run while changes are uncommitted.
func gated(c *command) bool { return c.Key == "apply" || c.Key == "deploy" }
