package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
	Git       bool     `json:"git"`
	Files     []change `json:"files"`
	Branch    string   `json:"branch,omitempty"`
	Ahead     int      `json:"ahead"`
	HasRemote bool     `json:"has_remote"`
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

func (s *server) changes() changes {
	c := changes{Files: []change{}}
	if out, err := s.git(10*time.Second, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return c
	}
	c.Git = true
	out, _ := s.git(20*time.Second, "status", "--porcelain=v1", "--untracked-files=all")
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(l) > 3 {
			c.Files = append(c.Files, change{Status: strings.TrimSpace(l[:2]), Path: l[3:]})
		}
	}
	if b, err := s.git(5*time.Second, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		c.Branch = strings.TrimSpace(b)
	}
	if n, err := s.git(5*time.Second, "rev-list", "--count", "@{u}..HEAD"); err == nil {
		c.HasRemote = true
		c.Ahead = atoi(strings.TrimSpace(n))
	}
	return c
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func (s *server) handleChanges(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.changes())
}

// handleCommit commits every pending change under the operator's message.
// The UI shows the file list before this is called; nothing is staged by hemma
// itself, so what is committed is exactly what the operator reviewed.
func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
		Push    bool   `json:"push"`
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
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.changes()
	if !ch.Git {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "the repo is not a git repository"})
		return
	}
	if len(ch.Files) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "nothing to commit"})
		return
	}
	var log strings.Builder
	for _, step := range [][]string{{"add", "-A"}, {"commit", "--message=" + msg}} {
		out, err := s.git(time.Minute, step...)
		log.WriteString(out)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "output": log.String()})
			return
		}
	}
	if in.Push {
		out, err := s.git(2*time.Minute, "push")
		log.WriteString(out)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "output": "committed locally, but push failed:\n" + log.String()})
			return
		}
	}
	log.Printf("commit:%d files (push=%v)", len(ch.Files), in.Push)
	writeJSON(w, 200, map[string]any{"ok": true, "output": log.String()})
}

// gated reports whether a command may not run while changes are uncommitted.
func gated(c *command) bool { return c.Key == "apply" || c.Key == "deploy" }
