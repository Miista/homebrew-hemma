package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeNested is write that also creates the parent directories.
func writeNested(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, path, content)
}

func TestValidExtraDirectivesPath(t *testing.T) {
	for _, ok := range []string{"extras/watch.caddy", "a.caddy", "./extras/watch.caddy", "a/b/../c.caddy"} {
		if err := ValidExtraDirectivesPath(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../outside.caddy", "a/../../outside.caddy", "..", "x\x00y"} {
		if err := ValidExtraDirectivesPath(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestLoadExtraDirectives_ReadsOnlyServicesThatAskForOne(t *testing.T) {
	root := t.TempDir()
	writeNested(t, filepath.Join(root, "extras", "watch.caddy"), "encode gzip\n")
	c := &Config{Services: map[string]Service{
		"watch": {FQDN: "watch.example.com", ExtraDirectives: "extras/watch.caddy"},
		"plain": {FQDN: "plain.example.com"},
	}}
	if failed := c.LoadExtraDirectives(root); failed != nil {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if got := c.ExtraDirectivesBodies["watch"]; got != "encode gzip\n" {
		t.Errorf("watch body = %q", got)
	}
	if _, ok := c.ExtraDirectivesBodies["plain"]; ok {
		t.Error("a service without extra_directives must have no body")
	}
	if c.ExtraDirectivesFailed != nil {
		t.Errorf("ExtraDirectivesFailed should be nil, got %v", c.ExtraDirectivesFailed)
	}
}

// A missing file, a bad path and an escape are each reported against the
// service and leave it WITHOUT a body — the caller keeps its existing site.
func TestLoadExtraDirectives_FailuresAreReportedPerService(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "good.caddy"), "encode gzip\n")
	c := &Config{Services: map[string]Service{
		"good":    {ExtraDirectives: "good.caddy"},
		"missing": {ExtraDirectives: "nope.caddy"},
		"escape":  {ExtraDirectives: "../outside.caddy"},
		"abs":     {ExtraDirectives: "/etc/hosts"},
	}}
	failed := c.LoadExtraDirectives(root)
	for _, name := range []string{"missing", "escape", "abs"} {
		if failed[name] == nil {
			t.Errorf("%s should have failed", name)
		}
		if _, ok := c.ExtraDirectivesBodies[name]; ok {
			t.Errorf("%s must have no body after a failure", name)
		}
	}
	if failed["good"] != nil || c.ExtraDirectivesBodies["good"] == "" {
		t.Errorf("a bad service must not take a good one down: failed=%v bodies=%v", failed, c.ExtraDirectivesBodies)
	}
	if !strings.Contains(failed["missing"].Error(), "nope.caddy") {
		t.Errorf("the error should name the file: %v", failed["missing"])
	}
	if len(c.ExtraDirectivesFailed) != 3 {
		t.Errorf("ExtraDirectivesFailed should hold the 3 failures, got %v", c.ExtraDirectivesFailed)
	}
}

// A lexically clean path can still lead out of the repo through a symlink; that
// must not be read into a generated config.
func TestLoadExtraDirectives_RefusesSymlinkOutOfTheRepo(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "respond \"leak\"\n")
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link.caddy")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := &Config{Services: map[string]Service{"s": {ExtraDirectives: "link.caddy"}}}
	failed := c.LoadExtraDirectives(root)
	if failed["s"] == nil || !strings.Contains(failed["s"].Error(), "outside the repository") {
		t.Fatalf("a symlink out of the repo must be refused, got %v", failed)
	}
	if _, ok := c.ExtraDirectivesBodies["s"]; ok {
		t.Error("the leaked file must not be loaded")
	}
}

// A symlink that stays inside the repo is fine (a shared extra reused by two services).
func TestLoadExtraDirectives_AllowsSymlinkInsideTheRepo(t *testing.T) {
	root := t.TempDir()
	writeNested(t, filepath.Join(root, "shared", "x.caddy"), "encode gzip\n")
	if err := os.Symlink(filepath.Join(root, "shared", "x.caddy"), filepath.Join(root, "link.caddy")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := &Config{Services: map[string]Service{"s": {ExtraDirectives: "link.caddy"}}}
	if failed := c.LoadExtraDirectives(root); failed != nil {
		t.Fatalf("in-repo symlink should load: %v", failed)
	}
}

// A second load starts clean: a file fixed since the last run is no longer a failure.
func TestLoadExtraDirectives_ResetsOnReload(t *testing.T) {
	root := t.TempDir()
	c := &Config{Services: map[string]Service{"s": {ExtraDirectives: "x.caddy"}}}
	if failed := c.LoadExtraDirectives(root); failed["s"] == nil {
		t.Fatal("expected a failure while the file is missing")
	}
	write(t, filepath.Join(root, "x.caddy"), "encode gzip\n")
	if failed := c.LoadExtraDirectives(root); failed != nil || c.ExtraDirectivesFailed != nil {
		t.Fatalf("the failure should clear once the file exists: %v / %v", failed, c.ExtraDirectivesFailed)
	}
}

func TestExtraDirectives_YAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "hosts: {}\ndomains: []\nservices:\n  watch:\n    fqdn: watch.example.com\n    host: h\n    backend: jellyfin:8096\n    extra_directives: extras/watch.caddy\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Services["watch"].ExtraDirectives; got != "extras/watch.caddy" {
		t.Fatalf("ExtraDirectives = %q after Load", got)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := again.Services["watch"].ExtraDirectives; got != "extras/watch.caddy" {
		t.Errorf("ExtraDirectives = %q after Save+Load: the field was dropped", got)
	}
}

// Cleared (empty) is omitted from the file, like every other optional field.
func TestExtraDirectives_EmptyIsOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "hosts: {}\ndomains: []\nservices:\n  a:\n    fqdn: a.example.com\n    host: h\n    backend: app:1\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "extra_directives") {
		t.Errorf("an unset extra_directives must not appear in services.yaml:\n%s", data)
	}
}
