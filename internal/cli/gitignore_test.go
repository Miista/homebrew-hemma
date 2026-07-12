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

// The unignore block must re-include sd's .conf/.caddy under data/ while
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

// --- access-control wiring advisories (authWiringWarnings) ---

// writeAuthComposeFixture writes the auth host's docker-compose.yml.
func writeAuthComposeFixture(t *testing.T, dir, composeYAML string) {
	t.Helper()
	mkdirs(t, dir, "appbox")
	if err := os.WriteFile(filepath.Join(dir, "appbox", "docker-compose.yml"), []byte(composeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthWiringWarnings_WiredThroughRepoLayout(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir) // grafana is oidc-with-groups -> artifact is planned
	writeAuthComposeFixture(t, dir, `services:
  authelia:
    environment:
      X_AUTHELIA_CONFIG: /config/configuration.yml,/config/hemma.access_control.generated.yml
`)
	cfg, code := loadExisting(filepath.Join(dir, configName), "test")
	if cfg == nil {
		t.Fatalf("load: %d", code)
	}
	if w := authWiringWarnings(dir, cfg); w != nil {
		t.Errorf("correctly wired -> silent, got %v", w)
	}
}

func TestAuthWiringWarnings_UnwiredWarns(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAuthComposeFixture(t, dir, "services:\n  authelia:\n    image: authelia/authelia\n")
	cfg, _ := loadExisting(filepath.Join(dir, configName), "test")
	w := authWiringWarnings(dir, cfg)
	if len(w) != 1 || !strings.Contains(w[0], "X_AUTHELIA_CONFIG") {
		t.Fatalf("want one unwired warning, got %v", w)
	}
}

func TestAuthWiringWarnings_GatedOnAuthService(t *testing.T) {
	// No auth_service at all -> silent.
	dir := t.TempDir()
	seed(t, dir)
	cfg, _ := loadExisting(filepath.Join(dir, configName), "test")
	if w := authWiringWarnings(dir, cfg); w != nil {
		t.Errorf("no auth_service -> silent, got %v", w)
	}

	// auth_service names a disabled service -> silent too (apply skips its
	// auth half the same way).
	dir2 := t.TempDir()
	content := `hosts:
  appbox: {ip: 192.0.2.2, dir: appbox}
domains:
  - example.com
defaults:
  dns_host: appbox
  auth_service: authelia
services:
  authelia:
    fqdn: auth.example.com
    host: appbox
    backend: authelia:9091
    disabled: true
  grafana:
    fqdn: grafana.example.com
    host: appbox
    backend: grafana:3000
    auth:
      mode: forward
      groups: [admins]
`
	if err := os.WriteFile(filepath.Join(dir2, configName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := loadExisting(filepath.Join(dir2, configName), "test")
	if w := authWiringWarnings(dir2, cfg2); w != nil {
		t.Errorf("disabled auth_service -> silent, got %v", w)
	}
}

func TestDoctor_ReportsWiringWarning(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAuthComposeFixture(t, dir, "services:\n  authelia:\n    image: authelia/authelia\n")
	// Bring generated files in sync so drift doesn't dominate the output.
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if !strings.Contains(out, "X_AUTHELIA_CONFIG: '/config/configuration.yml,/config/hemma.access_control.generated.yml'") {
		t.Errorf("doctor should surface the wiring recipe:\n%s", out)
	}
	// Advisory only: the wiring warning alone must not flip the exit code.
	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Errorf("wiring advisory must not affect exit code, got %d", code)
	}
}
