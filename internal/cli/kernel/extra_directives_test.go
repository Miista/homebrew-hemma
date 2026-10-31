package kernel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --extra-directives points a service at a file of Caddy directives that hemma copies into that
// service's generated site. The whole life of one: refused when bad (nothing saved), set,
// edited (doctor sees the drift, --fix regenerates), unreadable (the last good site is kept,
// never regenerated without its rules), and cleared.
func TestExtraDirectives_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	site := filepath.Join(dir, "appbox", "caddy", "data", "sites", "watch.caddy")
	extra := filepath.Join(dir, "extras", "watch.caddy")
	cfgPath := filepath.Join(dir, configName)
	readSite := func() string { b, _ := os.ReadFile(site); return string(b) }
	readCfg := func() string { b, _ := os.ReadFile(cfgPath); return string(b) }

	if code := Run([]string{"-C", dir, "add", "service", "watch", "--fqdn", "watch.example.com", "--host", "appbox", "--backend", "jellyfin:8096"}); code != 0 {
		t.Fatalf("add service: exit %d", code)
	}

	// bad values are usage errors and leave services.yaml exactly as it was
	before := readCfg()
	for _, bad := range []string{"/etc/hosts", "../outside.caddy", "extras/missing.caddy"} {
		if code := Run([]string{"-C", dir, "update", "service", "watch", "--extra-directives", bad}); code != 2 {
			t.Errorf("--extra-directives %q: want exit 2, got %d", bad, code)
		}
		if got := readCfg(); got != before {
			t.Fatalf("--extra-directives %q was refused but services.yaml changed", bad)
		}
	}
	// add checks it too, before creating the entry
	if code := Run([]string{"-C", dir, "add", "service", "other", "--fqdn", "other.example.com", "--host", "appbox", "--backend", "o:1", "--extra-directives", "nope.caddy"}); code != 2 {
		t.Errorf("add with a missing --extra-directives file: want exit 2, got %d", code)
	}
	if strings.Contains(readCfg(), "other.example.com") {
		t.Error("a refused add must not save the entry")
	}

	// set: the directives land in the site, before reverse_proxy
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("@m {\n\tpath /a\n}\nuri @m query x 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"-C", dir, "update", "service", "watch", "--extra-directives", "extras/watch.caddy"}); code != 0 {
		t.Fatalf("update --extra-directives: exit %d", code)
	}
	got := readSite()
	// inside one route block (so they run in written order, after any gate), two tabs deep, matcher indentation kept on top
	if !strings.Contains(got, "\t# extra_directives: extras/watch.caddy") ||
		!strings.Contains(got, "\troute {\n\t\t@m {\n\t\t\tpath /a\n\t\t}\n\t\turi @m query x 1\n\t}\n\treverse_proxy jellyfin:8096\n") {
		t.Fatalf("the directives are missing from the site, or not inside a route block:\n%s", got)
	}
	if strings.Index(got, "uri @m query x 1") > strings.Index(got, "reverse_proxy jellyfin:8096") {
		t.Errorf("the directives must come before reverse_proxy:\n%s", got)
	}
	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Errorf("doctor right after setting it: want exit 0, got %d", code)
	}

	// edit the file: the site is now out of date. doctor says so; --fix regenerates it
	if err := os.WriteFile(extra, []byte("encode gzip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Error("doctor should report the site as out of date after the extra file changed")
	}
	if code := Run([]string{"-C", dir, "doctor", "--fix"}); code != 0 {
		t.Fatalf("doctor --fix: exit %d", code)
	}
	if got := readSite(); !strings.Contains(got, "\t\tencode gzip\n") || strings.Contains(got, "uri @m") {
		t.Fatalf("--fix should have regenerated the site from the edited file:\n%s", got)
	}

	// unreadable: reported as an error, and the site on disk is kept, rules and all
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	kept := readSite()
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Error("doctor must report an unreadable extra_directives")
	}
	Run([]string{"-C", dir, "doctor", "--fix"})
	if got := readSite(); got != kept {
		t.Fatalf("an unreadable extra file must keep the last good site, not drop the rules:\n got: %q\nwant: %q", got, kept)
	}
	// ...and any other mutation (which syncs everything) fails loudly instead of dropping them
	if code := Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "d:1"}); code == 0 {
		t.Error("a sync that kept a site because its extra is unreadable must exit non-zero")
	}
	if got := readSite(); got != kept {
		t.Fatalf("the sync inside add must keep the site too:\n%s", got)
	}

	// cleared: the key goes from services.yaml and the directives from the site
	if code := Run([]string{"-C", dir, "update", "service", "watch", "--extra-directives", ""}); code != 0 {
		t.Fatalf("update --extra-directives '': exit %d", code)
	}
	if strings.Contains(readCfg(), "extra_directives") {
		t.Errorf("a cleared extra_directives must leave services.yaml:\n%s", readCfg())
	}
	if got := readSite(); strings.Contains(got, "extra_directives") || strings.Contains(got, "encode gzip") {
		t.Errorf("a cleared extra_directives must leave the site:\n%s", got)
	}
}

// A extra_directives file is read from inside the repository only.
func TestExtraDirectives_RefusesAbsoluteAndEscapingPaths(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	outside := filepath.Join(t.TempDir(), "x.caddy")
	if err := os.WriteFile(outside, []byte("respond \"hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, "../" + filepath.Base(filepath.Dir(outside)) + "/x.caddy"} {
		if code := Run([]string{"-C", dir, "add", "service", "s", "--fqdn", "s.example.com", "--host", "appbox", "--backend", "s:1", "--extra-directives", p}); code != 2 {
			t.Errorf("--extra-directives %q: want exit 2, got %d", p, code)
		}
	}
}
