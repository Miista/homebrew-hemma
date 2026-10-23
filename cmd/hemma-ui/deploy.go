package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"hemma/internal/cli/kernel"
)

// Deploy as a checklist. `hemma deploy` prints its phases as it goes (the kernel writes each line the moment it has
// it), so the page can show where a deploy is — "syncing hosts", "restarting services" — instead of a spinner that
// ends in a wall of text. hemma core is not changed for this: deployProgress reads the very lines deploy already
// prints and turns them into steps, tied to that wording by tests that feed it real deploy output.

type progItem struct {
	Title  string     `json:"title"`
	Status string     `json:"status"` // pending | running | done | failed
	Sub    []progItem `json:"sub,omitempty"`
}

type deployProgress struct {
	deploy bool // `deploy` (several phases) rather than `apply` (this server only)
	steps  []progItem
	phase  int // deploy: 0 syncing this host, 1 probing, 2 pulling, 3 validating sign-in, 4 applying
	failed bool
}

const (
	stepSync    = 0
	stepReach   = 1
	stepHosts   = 2
	stepAuth    = 3
	stepRestart = 4
)

var (
	hostHeaderRe = regexp.MustCompile(`^== (\S+) ==$`)
	sectionRe    = regexp.MustCompile(`^== (Validate|DNS|Caddy|Auth|Tunnel)\b`)
	summaryRe    = regexp.MustCompile(`^\s+(✓|✗) (\S+): \S+`)
	// what a section of `apply` does, in plain words
	sectionTitles = map[string]string{
		"Validate": "Checking the configuration", "DNS": "Restarting the DNS server", "Caddy": "Reloading the web proxy",
		"Auth": "Reloading sign-in", "Tunnel": "Restarting the tunnel",
	}
)

func newDeployProgress(command string) *deployProgress {
	p := &deployProgress{deploy: command == "deploy"}
	if p.deploy {
		p.steps = []progItem{
			{Title: "Syncing your changes", Status: "running"},
			{Title: "Checking the servers", Status: "pending"},
			{Title: "Syncing hosts", Status: "pending"},
			{Title: "Validating configuration(s)", Status: "pending"},
			{Title: "Restarting services", Status: "pending"},
		}
	} else {
		p.steps = []progItem{{Title: "Restarting services", Status: "running"}}
	}
	return p
}

// Steps is the checklist as the page shows it. A deploy is five steps and nothing more: which servers it is working
// through is detail nobody following along needs (the failure message still says where it stopped). Only `apply`
// keeps its sub-steps, because those are what it does (restarting DNS, reloading the proxy), not whose box it is.
func (p *deployProgress) Steps() []progItem {
	out := p.allSteps()
	if p.deploy {
		for i := range out {
			out[i].Sub = nil
		}
	}
	return out
}

// allSteps is a copy of the checklist with every sub-step, as it is tracked (a deploy's servers included).
func (p *deployProgress) allSteps() []progItem {
	out := make([]progItem, len(p.steps))
	for i, s := range p.steps {
		out[i] = s
		out[i].Sub = append([]progItem(nil), s.Sub...)
	}
	return out
}

// Feed reads one line of deploy's (or apply's) output.
func (p *deployProgress) Feed(line string) {
	line = strings.TrimRight(line, " \r")
	if strings.HasPrefix(line, "Error:") {
		p.fail()
		return
	}
	if !p.deploy {
		p.feedApply(line)
		return
	}
	switch {
	case strings.HasPrefix(line, "Deploying to "):
		p.steps[stepSync].Status = "done"
		p.steps[stepRestart].Sub = hostItems(line)
	case strings.HasPrefix(line, "Nothing to deploy"):
		p.finish()
	case line == "== Phase 0: probe ==":
		p.phase = 1
		p.steps[stepSync].Status = "done"
		p.steps[stepReach].Status = "running"
	case line == "== Phase 1: pull ==":
		p.phase = 2
		p.steps[stepReach].Status = "done"
		p.steps[stepHosts].Status = "running"
	case line == "== Phase 2: validate configuration ==":
		p.phase = 3
		p.steps[stepHosts].Status = "done"
		p.steps[stepAuth].Status = "running"
	case line == "== Phase 3: apply ==":
		p.phase = 4
		p.steps[stepHosts].Status = "done"
		p.steps[stepAuth].Status = "done"
		p.steps[stepRestart].Status = "running"
	case p.phase == 4 && line == "== Summary ==":
		p.endRunningSub("done")
	case p.phase == 4 && hostHeaderRe.MatchString(line):
		p.endRunningSub("done")
		p.setSub(hostHeaderRe.FindStringSubmatch(line)[1], "running")
	case p.phase == 4 && strings.Contains(line, "apply FAILED"):
		p.endRunningSub("failed")
	case p.phase == 4 && summaryRe.MatchString(line):
		m := summaryRe.FindStringSubmatch(line)
		st := "done"
		if m[1] == "✗" {
			st = "failed"
		}
		p.setSub(m[2], st)
	case line == "Deployed.":
		p.finish()
	}
}

func (p *deployProgress) feedApply(line string) {
	switch {
	case sectionRe.MatchString(line):
		p.endRunningSub("done")
		p.steps[0].Status = "running"
		p.steps[0].Sub = append(p.steps[0].Sub, progItem{Title: sectionTitles[sectionRe.FindStringSubmatch(line)[1]], Status: "running"})
	case line == "Applied.":
		p.finish()
	}
}

// hostItems reads the hosts out of "Deploying to 2 hosts: edge, core (here, last)."
func hostItems(line string) []progItem {
	i := strings.Index(line, ": ")
	if i < 0 {
		return nil
	}
	var out []progItem
	for _, n := range strings.Split(strings.TrimSuffix(strings.TrimSpace(line[i+2:]), "."), ", ") {
		n = strings.TrimSpace(n)
		if j := strings.Index(n, " ("); j >= 0 {
			n = n[:j]
		}
		if n != "" && n != "last)" && n != "here" {
			out = append(out, progItem{Title: n, Status: "pending"})
		}
	}
	return out
}

func (p *deployProgress) setSub(name, status string) {
	if !p.deploy {
		return
	}
	subs := p.steps[stepRestart].Sub
	for i := range subs {
		if subs[i].Title == name {
			subs[i].Status = status
		}
	}
}

// endRunningSub closes the sub-step that is running (a host whose apply just ended, or a section of apply) — unless it
// already failed.
func (p *deployProgress) endRunningSub(status string) {
	idx := stepRestart
	if !p.deploy {
		idx = 0
	}
	subs := p.steps[idx].Sub
	for i := range subs {
		if subs[i].Status == "running" {
			subs[i].Status = status
		}
	}
}

// fail marks what was running as failed. Later lines (a deploy that carries on to other hosts) cannot undo it.
func (p *deployProgress) fail() {
	p.failed = true
	marked := false
	for i := range p.steps {
		if p.steps[i].Status == "running" {
			p.steps[i].Status = "failed"
			marked = true
			for j := range p.steps[i].Sub {
				if p.steps[i].Sub[j].Status == "running" {
					p.steps[i].Sub[j].Status = "failed"
				}
			}
		}
	}
	if !marked {
		// nothing was running: the failure is in the first step that has not finished
		for i := range p.steps {
			if p.steps[i].Status != "done" {
				p.steps[i].Status = "failed"
				break
			}
		}
	}
}

func (p *deployProgress) finish() {
	for i := range p.steps {
		if p.steps[i].Status != "failed" {
			p.steps[i].Status = "done"
		}
		for j := range p.steps[i].Sub {
			if p.steps[i].Sub[j].Status != "failed" {
				p.steps[i].Sub[j].Status = "done"
			}
		}
	}
}

// Finish settles the checklist when the command has ended: a success completes everything, a failure marks whatever
// was in progress (or, failing that, the first unfinished step) as failed.
func (p *deployProgress) Finish(ok bool) {
	if ok {
		p.finish()
		return
	}
	if !p.failed {
		p.fail()
	}
}

// lineWriter hands every complete line written to it to onLine, and keeps everything for the final output.
type lineWriter struct {
	mu      sync.Mutex
	all     strings.Builder
	partial string
	onLine  func(string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all.Write(b)
	w.partial += string(b)
	for {
		i := strings.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := w.partial[:i]
		w.partial = w.partial[i+1:]
		w.onLine(line)
	}
	return len(b), nil
}

func (w *lineWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.all.String()
}

// handleDeploy runs `deploy` (or `apply`, on a repo with no origin) like any other command — same allowlist, same lock,
// same refusal of a dirty tree — but answers as a stream of events, so the page can show the checklist while it runs:
//
//	data: {"type":"progress","steps":[…]}      each time a step changes
//	data: {"type":"done","ok":true,"output":"…","steps":[…]}   once, at the end
//
// As with every command that changes something, it runs to the end even if the browser goes away.
func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Command string `json:"command"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || (in.Command != "deploy" && in.Command != "apply") {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "unknown command"})
		return
	}
	c := lookup(in.Command)
	argv, err := c.build(nil)
	flusher, canFlush := w.(http.Flusher)
	if err != nil || c == nil || !canFlush {
		writeJSON(w, 500, map[string]any{"ok": false, "output": "cannot start"})
		return
	}
	if err := s.needDeployHost(); err != nil {
		writeJSON(w, 409, map[string]any{"ok": false, "noDeployHost": true, "output": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidateChanges()
	if ch := s.changes(); len(ch.Files) > 0 {
		writeJSON(w, 409, map[string]any{"ok": false, "dirty": true,
			"output": "Refused: there are uncommitted changes. Commit them with a change message first."})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	prog := newDeployProgress(in.Command)
	var sendMu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		sendMu.Lock()
		defer sendMu.Unlock()
		fmt.Fprintf(w, "data: %s\n\n", b) // best effort: a browser that has gone away does not stop the deploy
		flusher.Flush()
	}
	stepsJSON := func() string { b, _ := json.Marshal(prog.Steps()); return string(b) }
	last := stepsJSON()
	send(map[string]any{"type": "progress", "steps": prog.Steps()})
	lw := &lineWriter{onLine: func(line string) {
		prog.Feed(line)
		if cur := stepsJSON(); cur != last {
			last = cur
			send(map[string]any{"type": "progress", "steps": prog.Steps()})
		}
	}}

	ctx, cancel := workContext(r, false, c.Timeout)
	defer cancel()
	code := s.inKernelTo(ctx, in.Command, lw, func(kenv *kernel.Env) int {
		code, known := kernel.Route(kenv, s.repo, filepath.Join(s.repo, "services.yaml"), argv[0], argv[1:])
		if !known {
			fmt.Fprintln(kenv.Err, "unknown command")
			return 2
		}
		return code
	})
	ok := code == 0
	if ctx.Err() != nil {
		ok = false
		fmt.Fprint(lw, "\n(timed out)\n")
	}
	prog.Finish(ok)
	log.Printf("run %q -> exit=%d (streamed)", in.Command, code)
	send(map[string]any{"type": "done", "ok": ok, "output": lw.String(), "steps": prog.Steps()})
}
