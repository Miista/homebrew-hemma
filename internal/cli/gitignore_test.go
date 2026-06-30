package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// The unignore block must re-include shd's .conf/.caddy under data/ while
// leaving runtime data (e.g. .db) ignored — verified against real git.
func TestUnignoreRules_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	gi := "**/data/**\n" + strings.Join(unignoreRules(), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gi), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"pi/pihole/data/dnsmasq.d", "optiplex/caddy/data/sites", "pi/pihole/data/data"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	tracked := []string{"pi/pihole/data/dnsmasq.d/x.generated.conf", "optiplex/caddy/data/sites/y.caddy"}
	ignoredStill := []string{"pi/pihole/data/data/gravity.db", "pi/pihole/data/dnsmasq.d/cache.db"}
	for _, f := range append(append([]string{}, tracked...), ignoredStill...) {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	ig, _ := ignoredPaths(dir, append(append([]string{}, tracked...), ignoredStill...))
	set := map[string]bool{}
	for _, p := range ig {
		set[p] = true
	}
	for _, f := range tracked {
		if set[f] {
			t.Errorf("%s should be tracked (un-ignored) but is ignored", f)
		}
	}
	for _, f := range ignoredStill {
		if !set[f] {
			t.Errorf("%s (runtime data) should stay ignored but is tracked", f)
		}
	}
}

func TestWriteManagedBlock_CreatesAndPreserves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")

	// Create from nothing.
	if err := writeManagedBlock(path, []string{"!a/", "!a/b/**"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, giBlockStart) || !strings.Contains(got, "!a/b/**") {
		t.Fatalf("block not written: %q", got)
	}

	// Pre-existing user content is preserved when we add to a file.
	os.WriteFile(path, []byte("*.tmp\ncerts/\n"), 0o644)
	if err := writeManagedBlock(path, []string{"!a/"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	got = string(b)
	if !strings.Contains(got, "*.tmp") || !strings.Contains(got, "certs/") {
		t.Errorf("user content not preserved: %q", got)
	}

	// Idempotent: replacing the block doesn't duplicate it.
	if err := writeManagedBlock(path, []string{"!a/"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if n := strings.Count(string(b), giBlockStart); n != 1 {
		t.Errorf("block duplicated: %d start markers", n)
	}
}
