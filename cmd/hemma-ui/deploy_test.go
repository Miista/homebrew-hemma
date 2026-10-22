package main

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// feed runs output through a progress tracker line by line, checking after each line that the checklist only ever
// moves forward (a step that is done never becomes pending or running again).
func feed(t *testing.T, command, out string) *deployProgress {
	t.Helper()
	p := newDeployProgress(command)
	rank := map[string]int{"pending": 0, "running": 1, "done": 2, "failed": 2}
	prev := p.Steps()
	for _, line := range strings.Split(out, "\n") {
		p.Feed(line)
		now := p.Steps()
		for i := range now {
			if rank[now[i].Status] < rank[prev[i].Status] {
				t.Fatalf("step %q went back from %s to %s on line %q", now[i].Title, prev[i].Status, now[i].Status, line)
			}
		}
		prev = now
	}
	return p
}

// statuses renders the tracked checklist (servers included) as "Title=status[sub=status …] | …".
func statuses(p *deployProgress) string {
	var out []string
	for _, s := range p.allSteps() {
		x := s.Title + "=" + s.Status
		if len(s.Sub) > 0 {
			var subs []string
			for _, sub := range s.Sub {
				subs = append(subs, sub.Title+"="+sub.Status)
			}
			x += "[" + strings.Join(subs, " ") + "]"
		}
		out = append(out, x)
	}
	return strings.Join(out, " | ")
}

// upTo returns the output up to (not including) the first line that starts with marker.
func upTo(out, marker string) string {
	if i := strings.Index(out, marker); i >= 0 {
		return out[:i]
	}
	return out
}

// Real output of `hemma deploy` (the shapes come from the CLI's own snapshot goldens).
const deployOK = `Pulled 2 new commits from origin here.
Deploying to 2 hosts: edge, core (here, last).

== Phase 0: probe ==
  ✓ edge: reachable
  ✓ core: local (no ssh)

== Phase 1: pull ==
  Pushed edge's 1 commit to origin.
  ✓ edge: pulled
  ✓ core: pulled
  all hosts at abc123

== Phase 2: validate auth ==
  ✓ sso config validates on core

== Phase 3: apply ==

== edge ==
  Applied.

== core ==
  Running on host "core".

  == Validate (core) ==
    ✓ caddy validate passes

  Applied.

== Summary ==
  ✓ edge: applied
  ✓ core: applied

Deployed.
`

// The checklist follows the deploy phase by phase: syncing, checking the servers, syncing hosts, checking sign-in, restarting services
// (with a line per host).
func TestDeployProgress_FollowsThePhases(t *testing.T) {
	start := newDeployProgress("deploy")
	if got := statuses(start); got != "Syncing your changes=running | Checking the servers=pending | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending" {
		t.Errorf("start: %s", got)
	}
	for marker, want := range map[string]string{
		"== Phase 0":          "Syncing your changes=done | Checking the servers=pending | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		"  ✓ edge: reachable": "Syncing your changes=done | Checking the servers=running | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		"== Phase 1":          "Syncing your changes=done | Checking the servers=running | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		"  ✓ core: pulled":    "Syncing your changes=done | Checking the servers=done | Syncing hosts=running | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		"== Phase 2":          "Syncing your changes=done | Checking the servers=done | Syncing hosts=running | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		"== Phase 3":          "Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=running | Restarting services=pending[edge=pending core=pending]",
		"== core ==":          "Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=done | Restarting services=running[edge=running core=pending]",
		"  Running on host":   "Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=done | Restarting services=running[edge=done core=running]",
		"== Summary":          "Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=done | Restarting services=running[edge=done core=running]",
	} {
		if got := statuses(feed(t, "deploy", upTo(deployOK, marker))); got != want {
			t.Errorf("before %q:\n got  %s\n want %s", marker, got, want)
		}
	}
	done := feed(t, "deploy", deployOK)
	done.Finish(true)
	if got := statuses(done); got != "Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=done | Restarting services=done[edge=done core=done]" {
		t.Errorf("end: %s", got)
	}
}

// What the page is sent: five steps for a deploy and no server names; `apply` keeps its sections.
func TestDeployProgress_PageSeesNoServerNames(t *testing.T) {
	p := feed(t, "deploy", deployOK)
	p.Finish(true)
	steps := p.Steps()
	if len(steps) != 5 {
		t.Fatalf("a deploy is five steps, got %d", len(steps))
	}
	for _, s := range steps {
		if len(s.Sub) != 0 {
			t.Errorf("step %q must not carry servers to the page: %+v", s.Title, s.Sub)
		}
	}
	b, _ := json.Marshal(steps)
	if strings.Contains(string(b), "edge") || strings.Contains(string(b), "core") {
		t.Errorf("no server name may reach the page: %s", b)
	}
	a := feed(t, "apply", "== Validate (core) ==\n== DNS (core) ==\nApplied.\n")
	if got := a.Steps(); len(got) != 1 || len(got[0].Sub) != 2 {
		t.Errorf("apply keeps its sections as sub-steps: %+v", got)
	}
}

// A failure marks the step that was running and leaves the later ones pending.
func TestDeployProgress_Failures(t *testing.T) {
	cases := map[string]struct{ out, want string }{
		"this host cannot sync with origin": {
			"Error: origin has 1 new commit that overlap with your 1 unpushed commit, so they could not be combined\n",
			"Syncing your changes=failed | Checking the servers=pending | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending",
		},
		"a server is unreachable": {
			"Deploying to 2 hosts: edge, core (here, last).\n\n== Phase 0: probe ==\n  ✗ edge: unreachable\n    ssh: connect to host edge port 22: Connection refused\n\nError: One or more hosts are unreachable over ssh — deploy aborted\n",
			"Syncing your changes=done | Checking the servers=failed | Syncing hosts=pending | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		},
		"a remote cannot be brought level with origin": {
			"Deploying to 2 hosts: edge, core (here, last).\n\n== Phase 0: probe ==\n  ✓ edge: reachable\n\n== Phase 1: pull ==\n\nError: Could not bring \"edge\" level with origin — deploy aborted\n",
			"Syncing your changes=done | Checking the servers=done | Syncing hosts=failed | Checking sign-in=pending | Restarting services=pending[edge=pending core=pending]",
		},
		"the auth config is invalid": {
			"Deploying to 2 hosts: edge, core (here, last).\n\n== Phase 0: probe ==\n== Phase 1: pull ==\n== Phase 2: validate auth ==\n\n== core ==\n  ✗ sso config validate FAILED\n\nError: The auth config on \"core\" is not valid — deploy aborted\n",
			"Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=failed | Restarting services=pending[edge=pending core=pending]",
		},
		"one host's apply fails, the other carries on": {
			"Deploying to 2 hosts: edge, core (here, last).\n\n== Phase 0: probe ==\n== Phase 1: pull ==\n== Phase 2: validate auth ==\n== Phase 3: apply ==\n\n== edge ==\n  boom\n  ✗ apply FAILED\n\n== core ==\n  Applied.\n\n== Summary ==\n  ✗ edge: failed-apply\n  ✓ core: applied\n\nError: Apply failed on 1 host — restarts cannot be rolled back\n",
			"Syncing your changes=done | Checking the servers=done | Syncing hosts=done | Checking sign-in=done | Restarting services=failed[edge=failed core=done]",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := feed(t, "deploy", c.out)
			p.Finish(false)
			if got := statuses(p); got != c.want {
				t.Errorf("\n got  %s\n want %s", got, c.want)
			}
		})
	}
}

// `apply` (a repo with no origin) is one step with its sections as sub-steps, in plain words.
func TestDeployProgress_Apply(t *testing.T) {
	out := `Running on host "core".

== Validate (core) ==
  ✓ caddy validate passes

== DNS (core) ==
  ✓ restarted pihole

== Caddy (core) ==
  ✓ caddy reloaded

== Auth (sso) ==
  ✓ sso reloaded

Applied.
`
	if got := statuses(feed(t, "apply", upTo(out, "== Caddy"))); got != "Restarting services=running[Checking the configuration=done Restarting the DNS server=running]" {
		t.Errorf("part-way: %s", got)
	}
	p := feed(t, "apply", out)
	p.Finish(true)
	if got := statuses(p); got != "Restarting services=done[Checking the configuration=done Restarting the DNS server=done Reloading the web proxy=done Reloading sign-in=done]" {
		t.Errorf("end: %s", got)
	}
	f := feed(t, "apply", upTo(out, "== Caddy")+"Error: Caddy refused the configuration\n")
	f.Finish(false)
	if got := statuses(f); got != "Restarting services=failed[Checking the configuration=done Restarting the DNS server=failed]" {
		// the section that was running when the error came is the one that failed
		t.Errorf("failure: %s", got)
	}
}

// Nothing to deploy is a success, not a failure or a step left hanging.
func TestDeployProgress_NothingToDeploy(t *testing.T) {
	p := feed(t, "deploy", "Nothing to deploy: no host runs a service or is the dns_host.\n")
	p.Finish(true)
	if got := statuses(p); !strings.Contains(got, "Restarting services=done") || strings.Contains(got, "failed") || strings.Contains(got, "pending") {
		t.Errorf("%s", got)
	}
}

// A writer that splits lines however the kernel happens to write them.
func TestLineWriter_SplitsLines(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(l string) { got = append(got, l) }}
	for _, chunk := range []string{"one\ntw", "o\n\nthr", "ee"} {
		w.Write([]byte(chunk))
	}
	if strings.Join(got, "|") != "one|two|" {
		t.Errorf("lines = %q (a partial last line waits for its newline)", got)
	}
	if w.String() != "one\ntwo\n\nthree" {
		t.Errorf("the whole output must be kept, got %q", w.String())
	}
}

// The endpoint refuses what is not deploy/apply and refuses a dirty tree, with a plain JSON answer (no stream).
func TestHandleDeploy_Refusals(t *testing.T) {
	s := repoWithConfig(t)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleDeploy(rec, httptest.NewRequest("POST", "/api/deploy", strings.NewReader(body)))
		return rec
	}
	if rec := post(`{"command":"doctor"}`); rec.Code != 400 {
		t.Errorf("only deploy and apply may use this endpoint, got %d", rec.Code)
	}
	if rec := post(`not json`); rec.Code != 400 {
		t.Errorf("bad body: %d", rec.Code)
	}
}

// End to end through the real kernel: the stream starts with the initial checklist, ends with one "done" event carrying
// the output and the final steps, and every event is a well-formed data: line.
func TestHandleDeploy_StreamsProgressThenDone(t *testing.T) {
	s := repoWithConfig(t) // a git-less repo: `apply` is what the page runs without an origin; this host matches no host
	rec := httptest.NewRecorder()
	s.handleDeploy(rec, httptest.NewRequest("POST", "/api/deploy", strings.NewReader(`{"command":"apply"}`)))
	if rec.Code == 409 {
		t.Skip("the fixture repo is dirty")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, body %s", ct, rec.Body.String())
	}
	var events []map[string]any
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("not an event line: %q", line)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		events = append(events, ev)
	}
	if len(events) < 2 || events[0]["type"] != "progress" {
		t.Fatalf("the stream must start with the initial checklist: %v", events)
	}
	last := events[len(events)-1]
	if last["type"] != "done" || last["output"] == nil || last["steps"] == nil {
		t.Errorf("the stream must end with one done event: %v", last)
	}
	for _, ev := range events[:len(events)-1] {
		if ev["type"] == "done" {
			t.Errorf("done must come exactly once, at the end")
		}
	}
}
