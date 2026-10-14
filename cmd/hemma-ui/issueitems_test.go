package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// doctor's output, in the shapes it really prints (a finding with and without a glyph, its follow-up lines, steps with
// a continuation, and the closing note about advisories).
const sampleDoctorOutput = `✓ optiplex: Caddyfile imports hemma.generated.caddy
7 generated files ignored by git — they won't be committed or deployed:
  pi/authelia/data/config/hemma.access_control.generated.yml
  pi/caddy/data/hemma.auth.generated.caddy

Add to .gitignore to un-ignore them (or run 'hemma doctor --fix'):
  !**/authelia/data/config/
  !**/caddy/data/sites/

Run 'hemma doctor --fix' to add these entries automatically.
⚠ hemma's .gitignore block is out of date: this version of hemma writes narrower rules and no longer re-includes whole data/ directories.
  Run 'hemma doctor --fix' to rewrite it.
✗ 2 generated files out of sync with services.yaml:
  - missing: pi/caddy/data/sites/a.caddy
  - modified: pi/pihole/data/dnsmasq.d/a.generated.conf

Run 'hemma doctor --fix' to reconcile (regenerate missing/modified, remove orphaned).
⚠ pi: no known_hosts entry for its deploy destination (pi)
  'hemma deploy' uses 'ssh -o BatchMode=yes' and will fail host-key verification.
  fix: connect once interactively to accept and record the host key — ssh pi (verify the fingerprint before accepting.)
⚠ OIDC logins to grafana will fail — no client is registered for it
    service grafana is auth: oidc but no Authelia OIDC client registers a redirect_uri for https://grafana.home.example/.
    fix:  register the client in pi/authelia/data/config/configuration.yml
          ('hemma create app oidc grafana' prints a paste-in snippet)
    then: hemma apply

These advisories require manual edits (see their fix lines) — 'hemma doctor --fix' does not resolve them.
`

func TestParseDoctorItems_Shapes(t *testing.T) {
	items := parseDoctorItems(sampleDoctorOutput)
	byTitle := map[string]issueItem{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	if len(items) != 6 {
		t.Fatalf("want 6 items (the closing advisory note is not one), got %d: %+v", len(items), items)
	}

	if it := byTitle["optiplex: Caddyfile imports hemma.generated.caddy"]; it.Status != "ok" || it.Fixable {
		t.Errorf("a passed check: %+v", it)
	}
	// the glyph-less finding is a warning; the rules to add by hand and the "Run …" hint belong to it and are not shown
	ign := byTitle["7 generated files ignored by git — they won't be committed or deployed"]
	if ign.Status != "warn" || !ign.Fixable || len(ign.List) != 2 || len(ign.Steps) != 0 || ign.Summary != "" || len(ign.Notes) != 0 {
		t.Errorf("ignored files: want a fixable warning listing just the 2 files, got %+v", ign)
	}
	blk := byTitle["hemma's .gitignore block is out of date"]
	if blk.Status != "warn" || !blk.Fixable || !strings.HasPrefix(blk.Summary, "this version of hemma writes narrower rules") || len(blk.Notes) != 0 {
		t.Errorf("a long first sentence splits into a title and its explanation; the hint becomes the Fix button: %+v", blk)
	}
	drift := byTitle["2 generated files out of sync with services.yaml"]
	if drift.Status != "err" || !drift.Fixable || len(drift.List) != 2 || drift.List[0] != "missing: pi/caddy/data/sites/a.caddy" {
		t.Errorf("drift: %+v", drift)
	}
	// not fixable by doctor: the how-to is the item
	kh := byTitle["pi: no known_hosts entry for its deploy destination (pi)"]
	if kh.Status != "warn" || kh.Fixable || len(kh.Steps) != 1 || kh.Steps[0].Label != "fix" || !strings.Contains(kh.Steps[0].Text, "ssh pi") || !strings.Contains(kh.Summary, "'hemma deploy' uses") {
		t.Errorf("known_hosts: %+v", kh)
	}
	oidc := byTitle["OIDC logins to grafana will fail — no client is registered for it"]
	if oidc.Fixable || len(oidc.Steps) != 2 || oidc.Steps[0].Label != "fix" || oidc.Steps[1].Label != "then" || oidc.Steps[1].Text != "hemma apply" ||
		!strings.Contains(oidc.Steps[0].Text, "prints a paste-in snippet") || !strings.HasPrefix(oidc.Summary, "service grafana is auth") {
		t.Errorf("a step's continuation line joins the step; then: is its own step: %+v", oidc)
	}
}

// An empty or fully clean output gives an empty list, never a null (the page maps over it).
func TestParseDoctorItems_Empty(t *testing.T) {
	if items := parseDoctorItems(""); items == nil || len(items) != 0 {
		t.Errorf("empty output: %#v", items)
	}
	items := parseDoctorItems("✓ a\n✓ b\n")
	if len(items) != 2 || items[0].Status != "ok" || items[1].Title != "b" {
		t.Errorf("clean output: %+v", items)
	}
}

// The real doctor, over real situations: what the checklist shows must follow what doctor says, so a change to doctor's
// wording fails HERE instead of quietly dropping an item or offering a Fix button that does nothing.
func TestIssues_Items_FollowRealDoctor(t *testing.T) {
	s := repoWithConfig(t)
	t.Setenv("PATH", t.TempDir()) // no git: doctor skips its gitignore check
	items := func() (all []issueItem, fixable bool) {
		rec := httptest.NewRecorder()
		s.handleIssues(rec, httptest.NewRequest("GET", "/api/issues", nil))
		var j struct {
			Fixable bool        `json:"fixable"`
			Items   []issueItem `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatalf("%v\n%s", err, rec.Body.String())
		}
		return j.Items, j.Fixable
	}
	find := func(all []issueItem, part string) *issueItem {
		for i := range all {
			if strings.Contains(all[i].Title, part) {
				return &all[i]
			}
		}
		return nil
	}
	run := func(argv ...string) {
		t.Helper()
		if code, out := s.runKernel(context.Background(), argv); code > 1 {
			t.Fatalf("%v: %d\n%s", argv, code, out)
		}
	}

	// 1. a brand new repo: generated files are missing → a failing item that doctor --fix resolves
	all, fixable := items()
	if d := find(all, "out of sync"); d == nil || d.Status == "ok" || !d.Fixable || !fixable {
		t.Errorf("missing generated files should be a fixable failure (fixable=%v): %+v", fixable, all)
	}
	// 2. after --fix: nothing is fixable any more, and the deploy host's known_hosts is a failure with a how-to
	run("doctor", "--fix")
	all, fixable = items()
	kh := find(all, "known_hosts")
	if fixable || kh == nil || kh.Fixable || len(kh.Steps) == 0 || kh.Steps[0].Label != "fix" {
		t.Errorf("after --fix: want known_hosts as a by-hand how-to and nothing fixable (fixable=%v): %+v", fixable, all)
	}
	if d := find(all, "in sync"); d == nil || d.Status != "ok" {
		t.Errorf("the drift check should now be a passed item: %+v", all)
	}
	// 3. an oidc service with no Authelia client: a failure to fix by hand, with doctor's own steps
	run("add", "service", "sso-app", "--fqdn=sso-app.example.com", "--host=appbox", "--backend=app:80", "--auth-mode=oidc")
	all, _ = items()
	oidc := find(all, "OIDC")
	if oidc == nil || oidc.Fixable || oidc.Status == "ok" || len(oidc.Steps) == 0 {
		t.Errorf("an unregistered OIDC client is a by-hand failure with steps: %+v", all)
	}
	// every item that says it is fixable agrees with doctor's own global answer
	for _, it := range all {
		if it.Fixable && !doctorFixable("Run 'hemma doctor --fix'") {
			t.Errorf("inconsistent: %+v", it)
		}
	}
}
