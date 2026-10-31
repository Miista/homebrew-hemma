package kernel

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// routeConf is the shape of a proxy JSON config that this check reads: one server whose routes are keyed by host.
func routeConf(routes ...string) string {
	return `{"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[` + strings.Join(routes, ",") + `]}}}}}`
}

func hostRoute(host, handle string) string {
	return fmt.Sprintf(`{"match":[{"host":[%q]}],"handle":[%s],"terminal":true}`, host, handle)
}

// fakeProxyContainer puts a `docker` on PATH that answers the two questions the check asks inside the proxy container:
// `caddy adapt` (the on-disk config as JSON) and a curl of the admin API (what is running). An empty running config makes the
// curl fail, as it does when the admin API cannot be reached. It returns the file the fake logs every call to.
func fakeProxyContainer(t *testing.T, adapted, running string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script := `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$*" in
  *"caddy adapt"*) [ -n "$FAKE_ADAPT" ] && cat "$FAKE_ADAPT" || exit 1 ;;
  *curl*)          [ -n "$FAKE_RUNNING" ] && cat "$FAKE_RUNNING" || exit 22 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	log := filepath.Join(dir, "docker.log")
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_ADAPT", "")
	t.Setenv("FAKE_RUNNING", "")
	if adapted != "" {
		t.Setenv("FAKE_ADAPT", write("adapted.json", adapted))
	}
	if running != "" {
		t.Setenv("FAKE_RUNNING", write("running.json", running))
	}
	return log
}

// deployedCfg: media carries extra directives, docs and wiki import (auth), plain has neither, remote lives on another host.
// The source files are named without the proxy's own file extension on purpose: the texts under test must not name the product
// anywhere, and a path would hide that.
func deployedCfg() *config.Config {
	return &config.Config{
		Hosts:    map[string]config.Host{"appbox": {IP: "192.0.2.2"}, "other": {IP: "192.0.2.3"}},
		Defaults: config.Defaults{DNSHost: "appbox", AuthSnippet: "shared/auth.rules"},
		Services: map[string]config.Service{
			"media":  {FQDN: "media.example.com", Host: "appbox", Backend: "m:80", ExtraDirectives: "extras/media.rules"},
			"docs":   {FQDN: "docs.example.com", Host: "appbox", Backend: "d:80", Auth: config.Auth{Mode: config.AuthForward}},
			"wiki":   {FQDN: "wiki.example.com", Host: "appbox", Backend: "w:80", Auth: config.Auth{Mode: config.AuthForward}},
			"plain":  {FQDN: "plain.example.com", Host: "appbox", Backend: "p:80"},
			"remote": {FQDN: "remote.example.com", Host: "other", Backend: "r:80", ExtraDirectives: "extras/r.rules"},
		},
	}
}

func runDeployed(t *testing.T, cfg *config.Config, self string) []Finding {
	t.Helper()
	defer SetLocalHost(func(*config.Config) string { return self })()
	var out bytes.Buffer
	return diagDeployed(&Env{Out: &out, Err: &out, NoTerminal: true}, "/repo", cfg)
}

func idsOf(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.ID+"="+f.Status)
	}
	return out
}

// byID returns the finding of that kind on appbox, failing if there is none.
func byID(t *testing.T, fs []Finding, kind string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.ID == "deployed."+kind+":appbox" {
			return f
		}
	}
	t.Fatalf("no deployed.%s:appbox in %v", kind, idsOf(fs))
	return Finding{}
}

func noFinding(t *testing.T, fs []Finding, kind string) {
	t.Helper()
	for _, f := range fs {
		if f.ID == "deployed."+kind+":appbox" {
			t.Errorf("deployed.%s:appbox should not be there: %+v", kind, f)
		}
	}
}

// noProductName fails if anything a user reads in these findings names the proxy or explains how apply works: they say that a
// change on disk has not been deployed, and the one fix.
func noProductName(t *testing.T, fs []Finding) {
	t.Helper()
	for _, f := range fs {
		text := strings.ToLower(f.Title + "\n" + f.Summary + "\n" + strings.Join(f.Notes, "\n") + "\n" + strings.Join(f.lines, "\n"))
		for _, s := range f.Steps {
			text += "\n" + strings.ToLower(s.Text)
		}
		for _, bad := range []string{"caddy", "reload", "validates first", "watch files"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s says %q, which the user should not have to read:\n%s", f.ID, bad, text)
			}
		}
	}
}

const (
	hMedia = `{"handler":"headers","b":2,"a":1}`
	hDocs  = `{"handler":"authentication","x":1}`
	hWiki  = `{"handler":"authentication","y":1}`
	hPlain = `{"handler":"reverse_proxy"}`
)

// Everything deployed: each advisory says so — one for the auth snippet, one for each service with extra directives — even though
// the two renderings write their keys in a different order, which is how the real ones differ (verified against a live proxy: equal
// once the keys are sorted).
func TestDeployed_EachAdvisoryReportsDeployed(t *testing.T) {
	disk := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki), hostRoute("plain.example.com", hPlain))
	live := routeConf(
		`{"terminal":true,"handle":[{"a":1,"b":2,"handler":"headers"}],"match":[{"host":["media.example.com"]}]}`,
		hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki), hostRoute("plain.example.com", hPlain))
	fakeProxyContainer(t, disk, live)
	fs := runDeployed(t, deployedCfg(), "appbox")
	if len(fs) != 2 {
		t.Fatalf("want two findings (auth snippet, media's extra directives), got %v", idsOf(fs))
	}
	if a := byID(t, fs, "auth"); a.Status != "ok" || a.Title != "the auth snippet is deployed" {
		t.Errorf("auth snippet: %+v", a)
	}
	if e := byID(t, fs, "extra.media"); e.Status != "ok" || e.Title != "extra directives for media are deployed" {
		t.Errorf("extra directives: %+v", e)
	}
	noProductName(t, fs)
}

// One message for a service whose extra directives are not deployed — whether the running proxy has an older copy or has never
// seen the site, which is not the user's business.
func TestDeployed_ExtraDirectivesChangedAndNotDeployed(t *testing.T) {
	for name, live := range map[string]string{
		"older copy running": routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)),
		"never loaded":       routeConf(hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)),
	} {
		disk := routeConf(hostRoute("media.example.com", `{"handler":"headers","new":true}`), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
		fakeProxyContainer(t, disk, live)
		fs := runDeployed(t, deployedCfg(), "appbox")
		if a := byID(t, fs, "auth"); a.Status != "ok" {
			t.Errorf("%s: the auth snippet is fine and must say so: %+v", name, a)
		}
		e := byID(t, fs, "extra.media")
		if e.Status != "warn" || e.Title != "extra directives for media changed on disk and not deployed" || e.Summary != "source: extras/media.rules" {
			t.Fatalf("%s: %+v", name, e)
		}
		if len(e.Steps) != 1 || e.Steps[0].Label != "fix" || e.Steps[0].Text != "hemma apply on appbox, or hemma deploy" {
			t.Errorf("%s: the fix is apply or deploy: %+v", name, e.Steps)
		}
		noProductName(t, fs)
	}
}

// The auth snippet is shared, so it is either deployed or it is not: one advisory, no list of services and no counts, however
// many gated services are behind.
func TestDeployed_AuthSnippetIsBinary(t *testing.T) {
	disk := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", `{"handler":"authentication","x":2}`), hostRoute("wiki.example.com", `{"handler":"authentication","y":2}`), hostRoute("plain.example.com", `{"handler":"other"}`))
	live := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki), hostRoute("plain.example.com", hPlain))
	fakeProxyContainer(t, disk, live)
	fs := runDeployed(t, deployedCfg(), "appbox")
	a := byID(t, fs, "auth")
	if a.Status != "warn" || a.Title != "the auth snippet changed on disk and not deployed" || a.Summary != "source: shared/auth.rules" {
		t.Fatalf("auth snippet: %+v", a)
	}
	for _, n := range []string{"docs", "wiki", "plain", " of ", "gated", "1", "2"} {
		if strings.Contains(a.Title+a.Summary+strings.Join(a.Notes, ""), n) {
			t.Errorf("the advisory about a shared file must not mention %q: %+v", n, a)
		}
	}
	if byID(t, fs, "extra.media").Status != "ok" {
		t.Error("media's extra directives are fine and must say so")
	}
	noProductName(t, fs)
	// and only ONE of the two gated services being behind is enough: the snippet is shared
	disk2 := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", `{"handler":"authentication","x":2}`), hostRoute("wiki.example.com", hWiki))
	live2 := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	fakeProxyContainer(t, disk2, live2)
	if a := byID(t, runDeployed(t, deployedCfg(), "appbox"), "auth"); a.Status != "warn" {
		t.Errorf("one gated service behind means the snippet is not deployed: %+v", a)
	}
}

// A service whose site is not on disk has nothing to compare, and the drift and import checks already report that: this check
// says nothing about it — not "not deployed", and above all not a false "deployed".
func TestDeployed_SiteNotOnDiskIsLeftToTheOtherChecks(t *testing.T) {
	for name, tc := range map[string]struct{ disk, live string }{
		"on the proxy only": {routeConf(hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)), routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))},
		"in neither":        {routeConf(hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)), routeConf(hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))},
	} {
		fakeProxyContainer(t, tc.disk, tc.live)
		fs := runDeployed(t, deployedCfg(), "appbox")
		noFinding(t, fs, "extra.media")
		if len(fs) != 1 || fs[0].ID != "deployed.auth:appbox" || fs[0].Status != "ok" {
			t.Errorf("%s: only the auth snippet is judged: %v", name, idsOf(fs))
		}
	}
}

// A service with both features: while the snippet is deployed, a difference in its route can only be its extras; while the snippet
// is not, the deploy that fixes the snippet fixes this site too, so it is not reported on its own.
func TestDeployed_ServiceWithBothFeatures(t *testing.T) {
	cfg := deployedCfg()
	m := cfg.Services["media"]
	m.Auth = config.Auth{Mode: config.AuthForward}
	cfg.Services["media"] = m
	diskMedia := hostRoute("media.example.com", `{"handler":"headers","new":true}`)
	liveMedia := hostRoute("media.example.com", hMedia)

	// snippet deployed (docs and wiki, the clean samples, agree): media's own extras are the difference
	fakeProxyContainer(t, routeConf(diskMedia, hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)),
		routeConf(liveMedia, hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)))
	fs := runDeployed(t, cfg, "appbox")
	if byID(t, fs, "auth").Status != "ok" || byID(t, fs, "extra.media").Status != "warn" {
		t.Errorf("snippet deployed, media's extras not: %v", idsOf(fs))
	}

	// snippet not deployed: one advisory about it, none about media on top
	fakeProxyContainer(t, routeConf(diskMedia, hostRoute("docs.example.com", `{"handler":"authentication","x":2}`), hostRoute("wiki.example.com", hWiki)),
		routeConf(liveMedia, hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)))
	fs = runDeployed(t, cfg, "appbox")
	if byID(t, fs, "auth").Status != "warn" {
		t.Errorf("the snippet is not deployed: %v", idsOf(fs))
	}
	noFinding(t, fs, "extra.media")
}

// If every gated service has extra directives of its own there is no clean sample for the snippet, so nothing is said about it.
func TestDeployed_NoCleanSampleNothingSaidAboutTheSnippet(t *testing.T) {
	cfg := deployedCfg()
	for _, n := range []string{"docs", "wiki"} {
		s := cfg.Services[n]
		s.ExtraDirectives = "extras/" + n + ".rules"
		cfg.Services[n] = s
	}
	site := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	fakeProxyContainer(t, site, site)
	fs := runDeployed(t, cfg, "appbox")
	noFinding(t, fs, "auth")
	if len(fs) != 3 {
		t.Errorf("only the three services' extra directives: %v", idsOf(fs))
	}
}

// Without an auth snippet configured there is nothing shared to deploy, so there is no auth advisory — only the extras.
func TestDeployed_NoAuthSnippetNoAuthAdvisory(t *testing.T) {
	site := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs))
	fakeProxyContainer(t, site, site)
	cfg := deployedCfg()
	cfg.Defaults.AuthSnippet = ""
	fs := runDeployed(t, cfg, "appbox")
	if len(fs) != 1 || fs[0].ID != "deployed.extra.media:appbox" {
		t.Fatalf("only media's extra directives should be checked: %v", idsOf(fs))
	}
}

// Each service with extra directives has its own advisory: one can be fine while another is not.
func TestDeployed_OneAdvisoryPerServiceWithExtraDirectives(t *testing.T) {
	cfg := deployedCfg()
	cfg.Services["notes"] = config.Service{FQDN: "notes.example.com", Host: "appbox", Backend: "n:80", ExtraDirectives: "extras/notes.rules"}
	disk := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki), hostRoute("notes.example.com", `{"handler":"headers","v":2}`))
	live := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki), hostRoute("notes.example.com", `{"handler":"headers","v":1}`))
	fakeProxyContainer(t, disk, live)
	fs := runDeployed(t, cfg, "appbox")
	if byID(t, fs, "extra.media").Status != "ok" || byID(t, fs, "extra.notes").Status != "warn" {
		t.Errorf("media should be deployed and notes not: %v", idsOf(fs))
	}
	if Problems(fs) != 0 {
		t.Error("none of them counts")
	}
}

// The advisories print the way every other advisory does in the CLI — a blank line before, the headline after the ⚠, the source as
// a body line, then `fix:` — and a deployed one is a plain ✓ line, as "Auth config is consistent." is. Only warnings and errors
// reach the UI's Issues page (it drops everything else), so a deployed one must never carry either status.
func TestDeployed_PrintsLikeTheOtherAdvisories(t *testing.T) {
	disk := routeConf(hostRoute("media.example.com", `{"handler":"headers","new":true}`), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	live := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	fakeProxyContainer(t, disk, live)
	fs := runDeployed(t, deployedCfg(), "appbox")
	for _, f := range fs {
		if f.Status == "ok" && (f.advisory || len(f.lines) > 0 || len(f.Steps) > 0) {
			t.Errorf("a deployed one is only a ✓ line: %+v", f)
		}
		if f.Status != "ok" && !f.advisory {
			t.Errorf("a problem is an advisory in the shared grammar: %+v", f)
		}
	}
	var out bytes.Buffer
	printFindings(&Env{Out: &out, Err: &out, NoTerminal: true}, "/repo", fs)
	got := out.String()
	for _, want := range []string{
		tick + " the auth snippet is deployed\n",
		"\n" + warn + " extra directives for media changed on disk and not deployed\n",
		"    source: extras/media.rules\n",
		"    fix:  hemma apply on appbox, or hemma deploy\n",
		"These advisories require manual edits (see their fix: lines)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the CLI output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "caddy") {
		t.Errorf("the output names the proxy:\n%s", got)
	}
}

// When the proxy cannot be asked there is one soft finding about that — never a silent pass, never a failing exit, and not a pretend
// "deployed" for either advisory. It does not name the proxy either.
func TestDeployed_UnreachableIsASoftFinding(t *testing.T) {
	for name, tc := range map[string]struct{ adapted, running string }{
		"admin API down":  {routeConf(hostRoute("media.example.com", hMedia)), ""},
		"on-disk fails":   {"", routeConf(hostRoute("media.example.com", hMedia))},
		"unreadable JSON": {routeConf(hostRoute("media.example.com", hMedia)), "not json"},
	} {
		fakeProxyContainer(t, tc.adapted, tc.running)
		fs := runDeployed(t, deployedCfg(), "appbox")
		if len(fs) != 1 || fs[0].Status != "warn" || fs[0].ID != "deployed.unreachable:appbox" ||
			!strings.HasPrefix(fs[0].Title, "could not check whether changes on disk are deployed") {
			t.Errorf("%s: want one could-not-check finding, got %v", name, idsOf(fs))
		}
		if Problems(fs) != 0 {
			t.Errorf("%s: must not affect the exit code", name)
		}
		noProductName(t, fs)
	}
}

// The advisories never make doctor exit non-zero.
func TestDeployed_NeverCounts(t *testing.T) {
	fakeProxyContainer(t, routeConf(hostRoute("media.example.com", `{"handler":"new"}`), hostRoute("docs.example.com", `{"handler":"new"}`), hostRoute("wiki.example.com", hWiki)),
		routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki)))
	fs := runDeployed(t, deployedCfg(), "appbox")
	warned := 0
	for _, f := range fs {
		if f.Status == "warn" {
			warned++
		}
	}
	if warned != 2 || Problems(fs) != 0 {
		t.Errorf("two warnings that do not count: %v (problems %d)", idsOf(fs), Problems(fs))
	}
}

// Nothing to compare means nothing is asked: no docker call at all, so a host without these features pays nothing.
func TestDeployed_NothingToCheckAsksNothing(t *testing.T) {
	log := fakeProxyContainer(t, routeConf(), routeConf())
	cfg := deployedCfg()
	delete(cfg.Services, "media")
	delete(cfg.Services, "remote")
	cfg.Defaults.AuthSnippet = "" // no auth snippet: docs' (auth) import carries nothing hemma copies through
	if fs := runDeployed(t, cfg, "appbox"); fs != nil {
		t.Errorf("nothing to check, got %v", idsOf(fs))
	}
	// a machine that is not a managed host, and a host with no services, are skipped too
	if fs := runDeployed(t, deployedCfg(), ""); fs != nil {
		t.Errorf("unknown machine: got %v", idsOf(fs))
	}
	empty := deployedCfg()
	empty.Services = map[string]config.Service{}
	if fs := runDeployed(t, empty, "appbox"); fs != nil {
		t.Errorf("host without services: got %v", idsOf(fs))
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker must not be called when there is nothing to check: %q", b)
	}
}

// Each host is checked by its own doctor: another host's services are not looked up in this host's proxy.
func TestDeployed_OnlyThisHostsServices(t *testing.T) {
	site := routeConf(hostRoute("media.example.com", hMedia), hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	fakeProxyContainer(t, site, site)
	// "remote" has extra directives but lives on another host and is in neither config: it must not be reported
	for _, f := range runDeployed(t, deployedCfg(), "appbox") {
		if strings.Contains(f.ID, "remote") || f.Status != "ok" {
			t.Fatalf("unexpected %+v", f)
		}
	}
}

// A disabled service has no site, so it is not expected anywhere and has no advisory.
func TestDeployed_DisabledServiceIsSkipped(t *testing.T) {
	site := routeConf(hostRoute("docs.example.com", hDocs), hostRoute("wiki.example.com", hWiki))
	fakeProxyContainer(t, site, site)
	cfg := deployedCfg()
	m := cfg.Services["media"]
	m.Disabled = true
	cfg.Services["media"] = m
	fs := runDeployed(t, cfg, "appbox")
	if len(fs) != 1 || fs[0].ID != "deployed.auth:appbox" || fs[0].Status != "ok" {
		t.Fatalf("got %v", idsOf(fs))
	}
}
