package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
}

func TestIgnoredPaths_DetectsDataRule(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("**/data/**\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"optiplex/caddy/data/sites/x.caddy", // ignored
		"optiplex/README.md",                // not ignored
	}
	ignored, ok := ignoredPaths(dir, paths)
	if !ok {
		t.Fatal("check should have run")
	}
	if len(ignored) != 1 || ignored[0] != "optiplex/caddy/data/sites/x.caddy" {
		t.Errorf("expected the data/ path ignored, got %v", ignored)
	}
}

func TestIgnoredPaths_NotARepo(t *testing.T) {
	// A bare temp dir is not a git work tree -> check can't run -> ok=false.
	if _, ok := ignoredPaths(t.TempDir(), []string{"a/b"}); ok {
		t.Error("expected ok=false outside a git repo")
	}
}

func TestUnignoreSuggestions_ParentChain(t *testing.T) {
	got := unignoreSuggestions([]string{"pi/pihole/data/dnsmasq.d/generated/x.conf"})
	rules := got["pi"]
	// Must un-ignore each parent dir top-down before the leaf glob.
	want := []string{
		"!pihole/",
		"!pihole/data/",
		"!pihole/data/dnsmasq.d/",
		"!pihole/data/dnsmasq.d/generated/",
		"!pihole/data/dnsmasq.d/generated/**",
	}
	if len(rules) != len(want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rule[%d] = %q, want %q", i, rules[i], want[i])
		}
	}
}
