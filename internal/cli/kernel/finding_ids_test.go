package kernel

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hemma/internal/auth"
	"hemma/internal/config"
)

// A finding's ID names WHAT it is about, never who reported it: <type>:<scope>:<subject>, where scope is the host (folder) the
// subject belongs to, or "repo" when it belongs to none, and subject is the service, file or setting. The same problem found
// from two machines has the same ID, which is what lets the UI show it once. These tests hold that scheme down before anything
// builds on it.

var idShape = regexp.MustCompile(`^[a-z][a-z0-9._-]*:[A-Za-z0-9._-]+:[A-Za-z0-9._/@-]+$`)

// problemRepo is a repo with a mix of every kind of problem, so every kind of finding exists in it.
func problemRepo(t *testing.T) (dir, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	gitInit(t, dir)
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	for _, n := range []string{"alpha", "beta", "gamma"} {
		Run([]string{"-C", dir, "add", "service", n, "--fqdn", n + ".example.com", "--host", "appbox", "--backend", n + ":80"})
	}
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("**/sites/gamma.caddy\n\n# >>> hemma managed >>>\n!**/data/\n!**/data/**/\n# <<< hemma managed <<<\n\n!**/data/\n"), 0o644)
	cf := filepath.Join(dir, "appbox", "caddy", "data", "Caddyfile")
	os.MkdirAll(filepath.Dir(cf), 0o755)
	os.WriteFile(cf, []byte("{\n}\nimport splitdns.generated.caddy\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "resolver", "caddy", "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "resolver", "caddy", "data", "Caddyfile"), []byte("{\n}\n"), 0o644)
	sites := filepath.Join(dir, "appbox", "caddy", "data", "sites")
	f, _ := os.OpenFile(filepath.Join(sites, "alpha.caddy"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("# edited by hand\n")
	f.Close()
	os.Remove(filepath.Join(sites, "beta.caddy"))
	return dir, filepath.Join(dir, configName)
}

func diagnoseAs(t *testing.T, cfgPath, self string) []Finding {
	t.Helper()
	defer SetLocalHost(func(*config.Config) string { return self })()
	fs, err := Diagnose(stdEnv(), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func idList(fs []Finding) []string {
	var ids []string
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	return ids
}

// Every ID has the same three parts, none is empty, and none repeats within one run: a repeat would make two cards one.
func TestFindingIDs_WellFormedAndUnique(t *testing.T) {
	_, cfgPath := problemRepo(t)
	fs := diagnoseAs(t, cfgPath, "appbox")
	if reported(fs) < 4 {
		t.Fatalf("the fixture should be full of problems, got %v", idList(fs))
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !idShape.MatchString(f.ID) {
			t.Errorf("ID %q (%s) is not <type>:<scope>:<subject>", f.ID, f.Title)
		}
		if seen[f.ID] {
			t.Errorf("ID %q appears twice", f.ID)
		}
		seen[f.ID] = true
	}
}

// The ID is the same whichever machine asks, and on every run, in the same order.
func TestFindingIDs_IndependentOfTheReportingHost(t *testing.T) {
	_, cfgPath := problemRepo(t)
	want := strings.Join(idList(diagnoseAs(t, cfgPath, "appbox")), "\n")
	for _, self := range []string{"resolver", "", "somewhere-else"} {
		if got := strings.Join(idList(diagnoseAs(t, cfgPath, self)), "\n"); got != want {
			t.Errorf("as %q the IDs differ:\n%s\n--\nwant\n%s", self, got, want)
		}
	}
	for i := 0; i < 10; i++ {
		if got := strings.Join(idList(diagnoseAs(t, cfgPath, "appbox")), "\n"); got != want {
			t.Fatalf("run %d: IDs or their order changed:\n%s\n--\n%s", i, got, want)
		}
	}
	for _, id := range strings.Split(want, "\n") {
		if strings.Contains(id, "resolver:") && strings.HasPrefix(id, "resolver:") {
			t.Errorf("ID %q starts with a host: the type comes first", id)
		}
	}
}

// Drift is one finding per generated file, keyed by the host folder it lives in and the file: fixing one file, or adding
// another, never renames the rest.
func TestFindingIDs_DriftIsPerFile(t *testing.T) {
	_, cfgPath := problemRepo(t)
	fs := diagnoseAs(t, cfgPath, "appbox")
	drift := map[string]Finding{}
	for _, f := range fs {
		if strings.HasPrefix(f.ID, "drift:") {
			drift[f.ID] = f
		}
	}
	for _, id := range []string{"drift:appbox:appbox/caddy/data/sites/alpha.caddy", "drift:appbox:appbox/caddy/data/sites/beta.caddy"} {
		f, ok := drift[id]
		if !ok {
			t.Fatalf("no %s in %v", id, idList(fs))
		}
		if f.Status != "err" || !f.Fixable {
			t.Errorf("%s: drift is an error and fixable, got %s fixable=%v", id, f.Status, f.Fixable)
		}
	}
	if !strings.Contains(drift["drift:appbox:appbox/caddy/data/sites/alpha.caddy"].Title, "modified") ||
		!strings.Contains(drift["drift:appbox:appbox/caddy/data/sites/beta.caddy"].Title, "missing") {
		t.Errorf("each says how its file is out of sync: %v / %v", drift["drift:appbox:appbox/caddy/data/sites/alpha.caddy"].Title, drift["drift:appbox:appbox/caddy/data/sites/beta.caddy"].Title)
	}
	// one file less out of sync: the other keeps its ID
	alpha := filepath.Join(filepath.Dir(cfgPath), "appbox", "caddy", "data", "sites", "alpha.caddy")
	b, _ := os.ReadFile(alpha)
	os.WriteFile(alpha, []byte(strings.TrimSuffix(string(b), "# edited by hand\n")), 0o644) // the hand edit undone
	after := diagnoseAs(t, cfgPath, "appbox")
	var left []string
	for _, f := range after {
		if strings.HasPrefix(f.ID, "drift:") && f.Status != "ok" {
			left = append(left, f.ID)
		}
	}
	if strings.Join(left, ",") != "drift:appbox:appbox/caddy/data/sites/beta.caddy" {
		t.Errorf("only beta should be left, got %v", left)
	}
}

// The CLI keeps printing one drift block; the per-file findings are for the UI and print nothing of their own, and the
// aggregate never reaches a UI or a remote doctor's JSON.
func TestFindingIDs_DriftPrintsOnceAndTheAggregateIsNotShared(t *testing.T) {
	dir, cfgPath := problemRepo(t)
	fs := diagnoseAs(t, cfgPath, "appbox")
	for _, f := range fs {
		if f.cliOnly {
			t.Errorf("Diagnose (the UI's and --json's findings) must not carry the CLI aggregate: %s", f.ID)
		}
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if n := strings.Count(out, "generated files out of sync with services.yaml"); n != 1 {
		t.Errorf("one drift block in the CLI, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "modified: appbox/caddy/data/sites/alpha.caddy"); n != 1 {
		t.Errorf("each drifted file is listed once, got %d:\n%s", n, out)
	}
	jsonOut := captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--json"}) })
	if strings.Contains(jsonOut, `"id":"drift:repo:all"`) || strings.Contains(jsonOut, "generated files out of sync") {
		t.Errorf("--json carries the per-file findings, not the CLI aggregate:\n%s", jsonOut)
	}
}

// Fixing every drift finding repairs the repo once and says so once.
func TestFindingIDs_FixingDriftReportsOnce(t *testing.T) {
	dir, _ := problemRepo(t)
	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })
	if n := strings.Count(out, "Reconciled"); n != 1 {
		t.Errorf("one Reconciled line, got %d:\n%s", n, out)
	}
	fs := diagnoseAs(t, filepath.Join(dir, configName), "appbox")
	for _, f := range fs {
		if strings.HasPrefix(f.ID, "drift:") && f.Status != "ok" {
			t.Errorf("%s is still out of sync after --fix", f.ID)
		}
	}
}

// The auth advisories are named by what they are about, not by their position: an advisory that goes away does not renumber
// the ones after it.
func TestFindingIDs_AuthAdvisoriesAreNotNumbered(t *testing.T) {
	cfg := &config.Config{
		Hosts:    map[string]config.Host{"appbox": {IP: "192.0.2.2"}},
		Defaults: config.Defaults{AuthSnippet: "auth.rules", AuthService: "authy"},
		Services: map[string]config.Service{
			"authy": {FQDN: "auth.example.com", Host: "appbox", Backend: "a:80"},
			"one":   {FQDN: "one.example.com", Host: "appbox", Backend: "o:80", Auth: config.Auth{Mode: config.AuthForward, Policy: config.AuthPolicy{Local: config.PolicyBypass, Public: config.PolicyBypass}}},
			"two":   {FQDN: "two.example.com", Host: "appbox", Backend: "t:80", Auth: config.Auth{Mode: config.AuthForward, Policy: config.AuthPolicy{Local: config.PolicyBypass, Public: config.PolicyBypass}}},
		},
	}
	idsFor := func() []string {
		var ids []string
		for _, f := range diagAuth(t.TempDir(), cfg) {
			if f.advisory {
				ids = append(ids, f.ID)
			}
		}
		return ids
	}
	before := idsFor()
	has := func(ids []string, id string) bool {
		for _, i := range ids {
			if i == id {
				return true
			}
		}
		return false
	}
	if !has(before, "auth.bypass.both:appbox:one") || !has(before, "auth.bypass.both:appbox:two") {
		t.Fatalf("each flagged service names itself: %v", before)
	}
	delete(cfg.Services, "one")
	after := idsFor()
	if !has(after, "auth.bypass.both:appbox:two") || has(after, "auth.bypass.both:appbox:one") {
		t.Errorf("two keeps its ID when one goes: %v", after)
	}
	for _, id := range before {
		if regexp.MustCompile(`:[0-9]+$`).MatchString(id) {
			t.Errorf("%q ends in a position, not a subject", id)
		}
	}
}

// Every advisory the auth provider and the kernel can raise carries a code and a subject: an advisory without them would get
// no ID, and a missing ID is a card that can never be merged or remembered.
func TestFindingIDs_EveryAdvisoryHasACodeAndASubject(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{
		Hosts:    map[string]config.Host{"appbox": {IP: "192.0.2.2"}},
		Defaults: config.Defaults{AuthSnippet: "auth.rules"},
		Services: map[string]config.Service{
			"web": {FQDN: "web.example.com", Host: "appbox", Backend: "w:80", Auth: config.Auth{Mode: config.AuthOIDC}},
		},
	}
	var advs []auth.Advisory
	advs = append(advs, authConfigWarnings(repo, cfg)...)
	cfg.Defaults.AuthService = "web"
	advs = append(advs, authConfigWarnings(repo, cfg)...)
	cfg.Defaults.AuthService = "missing"
	advs = append(advs, authConfigWarnings(repo, cfg)...)
	cfg.Defaults.AuthSnippet = ""
	cfg.Defaults.AuthService = "web"
	advs = append(advs, authConfigWarnings(repo, cfg)...)
	if len(advs) == 0 {
		t.Fatal("expected advisories to inspect")
	}
	for _, a := range advs {
		if a.Code == "" || a.Subject == "" {
			t.Errorf("advisory %q has code %q subject %q", a.Headline, a.Code, a.Subject)
		}
	}
}

// The other families of finding follow the same shape, host-first in scope.
func TestFindingIDs_OtherFamilies(t *testing.T) {
	_, cfgPath := problemRepo(t)
	fs := diagnoseAs(t, cfgPath, "appbox")
	want := []string{
		"gitignore:repo:generated",
		"gitignore:repo:block",
		"gitignore:repo:blanket",
		"caddy.import:appbox:Caddyfile",
		"caddy.import:resolver:Caddyfile",
		"caddy.trusted:appbox:Caddyfile",
		"caddy.trusted:resolver:Caddyfile",
	}
	got := map[string]bool{}
	for _, id := range idList(fs) {
		got[id] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("missing %s in %v", id, idList(fs))
		}
	}
}

// Findings of the other checks, built directly.
func TestFindingIDs_DeployedAndFleet(t *testing.T) {
	for kind, want := range map[string]string{
		deployedAuth:        "deployed.auth:appbox:snippet",
		"extra.media":       "deployed.extra:appbox:media",
		deployedUnreachable: "deployed.unreachable:appbox:proxy",
	} {
		if got := deployedID("appbox", kind); got != want {
			t.Errorf("deployedID(%q) = %q, want %q", kind, got, want)
		}
		if !idShape.MatchString(deployedID("appbox", kind)) {
			t.Errorf("%q is not well formed", deployedID("appbox", kind))
		}
	}
}

// ── merging the fleet's findings ──────────────────────────────────────────────────────────────────────────────────────

func finding(id, status, title string) Finding {
	return Finding{ID: id, Status: status, Title: title, Summary: "s", List: []string{"l"}, Steps: []FindingStep{{Label: "fix", Text: "do it"}}}
}

// The same finding from two hosts is one card that lists both hosts, and the fix stays with the host that can run it.
func TestMergeFleet_SameIDSameContentIsOneCard(t *testing.T) {
	a := finding("drift:optiplex:optiplex/x.caddy", "err", "modified: optiplex/x.caddy")
	local := []Finding{a.withFix(func(*Env) ([]string, error) { return nil, nil })}
	remote := []remoteResult{{host: "optiplex", fs: []Finding{finding("drift:optiplex:optiplex/x.caddy", "err", "modified: optiplex/x.caddy")}}}
	got := mergeFleet("pi", local, remote)
	if len(got) != 1 {
		t.Fatalf("want one card, got %d: %+v", len(got), got)
	}
	if strings.Join(got[0].Hosts, ",") != "pi,optiplex" {
		t.Errorf("hosts = %v, want pi,optiplex", got[0].Hosts)
	}
	if !got[0].Fixable {
		t.Errorf("the deploy host's own fix survives the merge")
	}
}

// A hand edit or a stale checkout on one host makes ITS finding different, and a difference stays visible as its own card.
func TestMergeFleet_DifferentContentStaysSeparate(t *testing.T) {
	local := []Finding{finding("drift:optiplex:optiplex/x.caddy", "err", "modified: optiplex/x.caddy")}
	other := finding("drift:optiplex:optiplex/x.caddy", "err", "missing:  optiplex/x.caddy")
	got := mergeFleet("pi", local, []remoteResult{{host: "optiplex", fs: []Finding{other}}})
	if len(got) != 2 {
		t.Fatalf("want two cards, got %d: %+v", len(got), got)
	}
	if got[0].Hosts[0] != "pi" || got[1].Hosts[0] != "optiplex" || len(got[0].Hosts) != 1 || len(got[1].Hosts) != 1 {
		t.Errorf("each card names the one host it came from: %v / %v", got[0].Hosts, got[1].Hosts)
	}
	if got[0].ID == got[1].ID {
		t.Errorf("two cards must not share a key: %q", got[0].ID)
	}
}

// An ID that is not in the current scheme (an older hemma on that host) is never merged, even when everything else is equal.
func TestMergeFleet_OldFormatIDsNeverMerge(t *testing.T) {
	for _, old := range []string{"drift", "caddy.import:optiplex", "auth.advisory:1", "deployed.auth:appbox", ""} {
		local := []Finding{finding(old, "err", "same")}
		got := mergeFleet("pi", local, []remoteResult{{host: "optiplex", fs: []Finding{finding(old, "err", "same")}}})
		if len(got) != 2 {
			t.Errorf("old ID %q merged: %+v", old, got)
		}
		if len(got) == 2 && got[0].ID == got[1].ID {
			t.Errorf("old ID %q: both cards keep the same key %q", old, got[0].ID)
		}
	}
}

// What only one host reports stays on that host; a remote fixable finding is not fixable from here, and says where to fix it.
func TestMergeFleet_RemoteOnlyAndNotFixableFromHere(t *testing.T) {
	rf := finding("caddy.import:optiplex:Caddyfile", "err", "optiplex: Caddyfile missing")
	rf.Steps = nil
	rf.Fixable = true
	got := mergeFleet("pi", []Finding{finding("gitignore:repo:block", "warn", "block")}, []remoteResult{{host: "optiplex", fs: []Finding{rf}}})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	r := got[1]
	if r.Fixable {
		t.Errorf("a remote host's finding cannot be fixed from the UI")
	}
	if len(r.Steps) == 0 || !strings.Contains(r.Steps[0].Text, "hemma doctor --fix") || !strings.Contains(r.Steps[0].Text, "optiplex") {
		t.Errorf("it says where to fix it: %+v", r.Steps)
	}
	if strings.Join(r.Hosts, ",") != "optiplex" {
		t.Errorf("hosts = %v", r.Hosts)
	}
}

// Merging is the same whichever host answers first.
func TestMergeFleet_OrderOfRemotesDoesNotChangeTheMerge(t *testing.T) {
	mk := func(h string) remoteResult {
		return remoteResult{host: h, fs: []Finding{finding("gitignore:repo:block", "warn", "block")}}
	}
	local := []Finding{finding("gitignore:repo:block", "warn", "block")}
	a := mergeFleet("pi", local, []remoteResult{mk("b"), mk("c")})
	b := mergeFleet("pi", local, []remoteResult{mk("c"), mk("b")})
	if len(a) != 1 || len(b) != 1 || strings.Join(a[0].Hosts, ",") != "pi,b,c" || strings.Join(b[0].Hosts, ",") != "pi,b,c" {
		t.Errorf("hosts should come out in a fixed order: %v / %v", a[0].Hosts, b[0].Hosts)
	}
}

// ok findings are never shown, so they are not merged either (and never keep a card alive).
func TestMergeFleet_OKFindingsAreKeptPerHost(t *testing.T) {
	got := mergeFleet("pi", []Finding{{ID: "gitignore:repo:generated", Status: "ok", Title: "fine"}},
		[]remoteResult{{host: "optiplex", fs: []Finding{{ID: "gitignore:repo:generated", Status: "ok", Title: "fine"}}}})
	for _, f := range got {
		if len(f.Hosts) == 0 {
			t.Errorf("every finding names its host(s): %+v", f)
		}
	}
}
