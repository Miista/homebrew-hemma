package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// caddyFile returns the planned Caddy site file of a service.
func caddyFile(t *testing.T, p *Plan, name string) File {
	t.Helper()
	for _, f := range p.Files[name] {
		if strings.HasSuffix(f.Path, ".caddy") {
			return f
		}
	}
	t.Fatalf("no caddy file planned for %q: %+v", name, p.Files[name])
	return File{}
}

func withExtra() *config.Config {
	c := base()
	c.Services["watch"] = config.Service{FQDN: "watch.example.com", Host: "appbox", Backend: "jellyfin:8096", ExtraDirectives: "extras/watch.caddy"}
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "docsapp:8000"}
	return c
}

func TestBuild_RendersExtraDirectivesIntoThatServiceOnly(t *testing.T) {
	c := withExtra()
	c.ExtraDirectivesBodies = map[string]string{"watch": "encode gzip\n"}
	p := Build(c)
	if len(p.Skipped) != 0 {
		t.Fatalf("unexpected skips: %v", p.Skipped)
	}
	watch := caddyFile(t, p, "watch").Content
	if !strings.Contains(watch, "\t# extra_directives: extras/watch.caddy") || !strings.Contains(watch, "\tencode gzip\n") {
		t.Errorf("the extra directives are missing from the watch site:\n%s", watch)
	}
	if strings.Index(watch, "\tencode gzip") > strings.Index(watch, "\treverse_proxy jellyfin:8096") {
		t.Errorf("the extra must come before reverse_proxy:\n%s", watch)
	}
	if docs := caddyFile(t, p, "docs").Content; strings.Contains(docs, "extra_directives") || strings.Contains(docs, "encode") {
		t.Errorf("another service must not get the extra:\n%s", docs)
	}
}

// A service whose extra is not loaded (no body) renders as if it had none; it is
// the callers' job to pin it (below) so that is never what reaches disk.
func TestBuild_NoLoadedBodyRendersPlain(t *testing.T) {
	p := Build(withExtra())
	if got := caddyFile(t, p, "watch").Content; strings.Contains(got, "extra_directives") {
		t.Errorf("without a loaded body nothing should be rendered:\n%s", got)
	}
}

// The core guarantee: when the extra file cannot be read, the site on disk is
// kept as it is — the rule is not silently dropped by a regeneration.
func TestPinExtraDirectivesToDisk_KeepsTheLastGoodSite(t *testing.T) {
	c := withExtra()
	p := Build(c) // body not loaded: what a failed read leaves behind
	watch := caddyFile(t, p, "watch")

	root := t.TempDir()
	onDisk := "# last good\nwatch.example.com {\n\tencode gzip\n}\n"
	full := filepath.Join(root, watch.Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(onDisk), 0o644); err != nil {
		t.Fatal(err)
	}

	PinExtraDirectivesToDisk(p, root, map[string]error{"watch": errors.New("unreadable")})

	if got := caddyFile(t, p, "watch").Content; got != onDisk {
		t.Errorf("the planned site should be the one on disk:\n got: %q\nwant: %q", got, onDisk)
	}
}

// Only the failed service is pinned; the DNS file and every other site keep their planned content.
func TestPinExtraDirectivesToDisk_LeavesOthersAlone(t *testing.T) {
	p := Build(withExtra())
	docsBefore := caddyFile(t, p, "docs").Content
	var dnsBefore string
	for _, f := range p.Files["watch"] {
		if strings.HasSuffix(f.Path, ".conf") {
			dnsBefore = f.Content
		}
	}

	root := t.TempDir()
	for _, f := range append(append([]File{}, p.Files["watch"]...), p.Files["docs"]...) {
		full := filepath.Join(root, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("ON DISK\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	PinExtraDirectivesToDisk(p, root, map[string]error{"watch": errors.New("x")})

	if got := caddyFile(t, p, "docs").Content; got != docsBefore {
		t.Errorf("an unfailed service must keep its planned site: %q", got)
	}
	for _, f := range p.Files["watch"] {
		if strings.HasSuffix(f.Path, ".conf") && f.Content != dnsBefore {
			t.Errorf("the DNS file of a failed service must not be touched: %q", f.Content)
		}
	}
	if got := caddyFile(t, p, "watch").Content; got != "ON DISK\n" {
		t.Errorf("the failed service's site should be pinned: %q", got)
	}
}

// Nothing on disk yet means nothing to preserve: the planned content stays.
func TestPinExtraDirectivesToDisk_NoFileOnDiskKeepsPlanned(t *testing.T) {
	p := Build(withExtra())
	before := caddyFile(t, p, "watch").Content
	PinExtraDirectivesToDisk(p, t.TempDir(), map[string]error{"watch": errors.New("x")})
	if got := caddyFile(t, p, "watch").Content; got != before {
		t.Errorf("a site not yet on disk keeps its planned content: %q", got)
	}
}

func TestPinExtraDirectivesToDisk_NilIsANoop(t *testing.T) {
	p := Build(withExtra())
	before := caddyFile(t, p, "watch").Content
	PinExtraDirectivesToDisk(p, t.TempDir(), nil)
	if got := caddyFile(t, p, "watch").Content; got != before {
		t.Errorf("no failures must change nothing: %q", got)
	}
}
