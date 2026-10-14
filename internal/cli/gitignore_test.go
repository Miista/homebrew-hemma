package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
	"hemma/internal/plan"
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

// The block is FIXED (the same in every repo and at every moment) and NARROW: only the directories
// hemma writes into are re-included, one block covers every host, and nothing un-ignores directories
// wholesale (that would make git walk all runtime data on every `git status`).
func TestUnignoreRules_FixedNarrowAndHostAgnostic(t *testing.T) {
	want := []string{
		"!**/authelia/data/config/",
		"!**/authelia/data/config/*.generated.yml",
		"!**/caddy/data/*.generated.caddy",
		"!**/caddy/data/sites/",
		"!**/caddy/data/sites/*.caddy",
		"!**/caddy/data/tls/",
		"!**/caddy/data/tls/*.caddy",
		"!**/pihole/data/dnsmasq.d/",
		"!**/pihole/data/dnsmasq.d/*.generated.conf",
	}
	got := unignoreRules()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range got {
		if r == "!**/data/" || r == "!**/data/**/" {
			t.Errorf("blanket rule %q would make git walk every data dir", r)
		}
	}
}

// rulesForOutputs describes a path host-agnostically: from the service directory before data/, ignoring
// the host directory, and skipping paths that are not under a data/ directory at all.
func TestRulesForOutputs_HostAgnosticAndSkipsNonData(t *testing.T) {
	got := rulesForOutputs([]string{
		"pi/caddy/data/sites/a.caddy", "optiplex/caddy/data/sites/b.caddy", "nas-box/caddy/data/sites/c.caddy",
		"pi/cloudflared/config.yml", "shared/auth.caddy",
	})
	want := "!**/caddy/data/sites/\n!**/caddy/data/sites/*.caddy"
	if strings.Join(got, "\n") != want {
		t.Errorf("rules = %q, want %q", strings.Join(got, "\n"), want)
	}
	if len(rulesForOutputs(nil)) != 0 {
		t.Error("no output paths must give no rules")
	}
}

// The fixed block must cover everything hemma actually writes under a data/ directory. This builds a rich
// plan (caddy sites, tls, trusted proxies, the (auth) snippet, dnsmasq, the auth provider's artifact) and
// asks real git whether any of its outputs would be ignored under `**/data/**` plus the block — so adding
// a new kind of output without a rule fails here instead of silently not being deployed.
func TestUnignoreRules_CoversEveryPlanOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	mkdirs(t, dir, "appbox")
	seedWithAuth(t, dir)
	cfg, err := config.Load(filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	paths := planPaths(plan.Build(cfg))
	underData := 0
	for _, p := range paths {
		if !strings.Contains(p, "/data/") {
			continue
		}
		underData++
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if underData < 4 {
		t.Fatalf("expected a rich plan with several outputs under data/, got %d: %v", underData, paths)
	}
	gi := "**/data/**\n" + strings.Join(unignoreRules(), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gi), 0o644); err != nil {
		t.Fatal(err)
	}
	if ig, ok := ignoredPaths(dir, paths); !ok || len(ig) != 0 {
		t.Errorf("hemma writes these but the fixed rules leave them ignored (ok=%v):\n  %s", ok, strings.Join(ig, "\n  "))
	}
}

// The unignore block must re-include hemma's .conf/.caddy/.yml under data/ while
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
	for _, d := range []string{"pi/pihole/data/dnsmasq.d", "optiplex/caddy/data/sites", "pi/pihole/data/data", "pi/authelia/data/config", "pi/registry/data/docker"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	tracked := []string{"pi/pihole/data/dnsmasq.d/x.generated.conf", "optiplex/caddy/data/sites/y.caddy",
		"pi/authelia/data/config/hemma.access_control.generated.yml",
		"nas/caddy/data/sites/z.caddy"} // a host that is not in the plan's paths: the rules are host-agnostic
	os.MkdirAll(filepath.Join(dir, "nas/caddy/data/sites"), 0o755)
	ignoredStill := []string{"pi/pihole/data/data/gravity.db", "pi/pihole/data/dnsmasq.d/cache.db",
		"pi/authelia/data/config/db.sqlite3", "pi/registry/data/docker/blob.bin"}
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

// The check reports the missing import (its return value is the count), but
// cmdDoctor deliberately does NOT add it to the exit-code tally — see the
// call site. Enforcement is apply's job (TestApply-side coverage below).
func TestCheckTrustedProxiesImport_MissingReportsProblem(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "appbox", config.DefaultCaddyDataDir), 0o755)
	os.WriteFile(filepath.Join(dir, "appbox", config.DefaultCaddyDataDir, "Caddyfile"),
		[]byte("import hemma.generated.caddy\n"), 0o644)
	cfg := &config.Config{Hosts: map[string]config.Host{"appbox": {IP: "192.0.2.2"}}}

	if got := checkTrustedProxiesImport(dir, cfg, false); got != 1 {
		t.Fatalf("expected 1 reported problem for missing import, got %d", got)
	}
}

// The advisory must not break `doctor --fix`'s ability to reach a clean exit:
// hemma cannot write the hand-maintained global options block, so a missing
// import is reported but never counted toward the exit code.
func TestDoctor_TrustedProxiesImportDoesNotBlockCleanExit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seed(t, dir)
	// No Caddyfile carries the trusted-proxies import, yet doctor --fix must
	// still be able to exit 0 once everything it CAN fix is fixed.
	if code := Run([]string{"-C", dir, "doctor", "--fix"}); code != 0 {
		t.Fatalf("doctor --fix exit = %d, want 0 (trusted-proxies advisory must not gate)", code)
	}
}

func TestCheckTrustedProxiesImport_PresentIsSilent(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "appbox", config.DefaultCaddyDataDir), 0o755)
	os.WriteFile(filepath.Join(dir, "appbox", config.DefaultCaddyDataDir, "Caddyfile"),
		[]byte("{\n\tservers {\n\t\timport hemma.trusted_proxies.generated.caddy\n\t}\n}\n"), 0o644)
	cfg := &config.Config{Hosts: map[string]config.Host{"appbox": {IP: "192.0.2.2"}}}

	if got := checkTrustedProxiesImport(dir, cfg, false); got != 0 {
		t.Fatalf("expected 0 problems when import present, got %d", got)
	}
}

// The check is REPORT-ONLY: --fix must never touch the Caddyfile, because the
// global options block the import belongs in is hand-maintained territory
// (Caddy allows exactly one per file). The finding still surfaces on every
// mutation, so it stays discoverable without hemma editing that block.
func TestCheckTrustedProxiesImport_FixNeverEditsCaddyfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"no global block", "import hemma.generated.caddy\n"},
		{"existing global block", "{\n\tdebug\n}\n\nimport hemma.generated.caddy\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "appbox", config.DefaultCaddyDataDir), 0o755)
			cfPath := filepath.Join(dir, "appbox", config.DefaultCaddyDataDir, "Caddyfile")
			os.WriteFile(cfPath, []byte(tc.content), 0o644)
			cfg := &config.Config{Hosts: map[string]config.Host{"appbox": {IP: "192.0.2.2"}}}

			// Still a problem in --fix mode: it is not mechanically fixable.
			if got := checkTrustedProxiesImport(dir, cfg, true); got != 1 {
				t.Fatalf("expected the problem to persist under --fix, got %d", got)
			}
			b, _ := os.ReadFile(cfPath)
			if string(b) != tc.content {
				t.Fatalf("--fix must not modify the Caddyfile:\n got: %q\nwant: %q", string(b), tc.content)
			}
		})
	}
}

func TestCheckTrustedProxiesImport_NoCaddyfileSkipped(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Hosts: map[string]config.Host{"appbox": {IP: "192.0.2.2"}}}
	if got := checkTrustedProxiesImport(dir, cfg, false); got != 0 {
		t.Fatalf("host with no Caddyfile should be skipped, got %d problems", got)
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
	if len(w) != 1 || !strings.Contains(w[0].String(), "X_AUTHELIA_CONFIG") {
		t.Fatalf("want one unwired advisory, got %v", w)
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
	// Rendering: repo paths are shown repo-relative, container paths as-is.
	if !strings.Contains(out, "appbox/docker-compose.yml") || strings.Contains(out, dir+"/appbox/docker-compose.yml") {
		t.Errorf("advisory paths should be repo-relative:\n%s", out)
	}
	// The compiler-style shape: fix:/then: mini-grammar under the headline.
	if !strings.Contains(out, "    fix:  ") || !strings.Contains(out, "    then: hemma apply") {
		t.Errorf("advisory should use the fix:/then: grammar:\n%s", out)
	}
	// Advisory only: the wiring warning alone must not flip the exit code.
	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Errorf("wiring advisory must not affect exit code, got %d", code)
	}
}

// The advisory summary line: whenever instructive advisories print, doctor
// must say — once, at the end of the block — that --fix does not resolve
// them. In plain mode (so nobody reaches for --fix expecting it to) AND in
// --fix mode (so surviving advisories don't read as --fix having failed).
const advisorySummaryLine = "'hemma doctor --fix' does not resolve them"

func TestDoctor_AdvisorySummaryLine_Plain(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAuthComposeFixture(t, dir, "services:\n  authelia:\n    image: authelia/authelia\n")
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) }) // settle drift

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if n := strings.Count(out, advisorySummaryLine); n != 1 {
		t.Errorf("plain doctor should print the advisory summary line exactly once, got %d:\n%s", n, out)
	}
}

func TestDoctor_AdvisorySummaryLine_FixMode(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAuthComposeFixture(t, dir, "services:\n  authelia:\n    image: authelia/authelia\n")

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })
	if n := strings.Count(out, advisorySummaryLine); n != 1 {
		t.Errorf("doctor --fix should print the advisory summary line exactly once, got %d:\n%s", n, out)
	}
}

// When the ONLY findings are instructive advisories, doctor must not
// recommend running --fix — it can't touch them.
func TestDoctor_AdvisoriesOnly_NoFixHint(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAuthComposeFixture(t, dir, "services:\n  authelia:\n    image: authelia/authelia\n")
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) }) // settle drift

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if !strings.Contains(out, "X_AUTHELIA_CONFIG") {
		t.Fatalf("fixture should produce a wiring advisory:\n%s", out)
	}
	if strings.Contains(out, "Run 'hemma doctor --fix'") {
		t.Errorf("advisories-only output must not recommend --fix:\n%s", out)
	}
	// No summary-free advisories: absence of fixable problems still ends the
	// advisory block with the summary line.
	if !strings.Contains(out, advisorySummaryLine) {
		t.Errorf("advisory block should end with the summary line:\n%s", out)
	}
}

// An older hemma wrote broad rules into its managed block. doctor notices a block that is not what
// it would write now — even when every generated file is already tracked (so nothing is "ignored")
// — and --fix rewrites it in place, leaving the rest of the file alone.
func TestCheckManagedBlock_StaleBlockIsReportedAndFixed(t *testing.T) {
	old := "*.env\n**/data/**\n" +
		giBlockStart + "\n# old comment\n!**/data/\n!**/data/**/\n!**/data/**/*.generated.conf\n!**/caddy/data/**/*.caddy\n" + giBlockEnd + "\n" +
		"# hand-written, after the block\n!pi/keep/me\n"
	gi := filepath.Join(t.TempDir(), ".gitignore")
	if err := os.WriteFile(gi, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	var problems int
	out := captureStdout(t, func() { problems = checkManagedBlock(gi, false) })
	if problems != 1 || !strings.Contains(out, "out of date") {
		t.Fatalf("stale block without --fix: problems=%d out=%q", problems, out)
	}
	if b, _ := os.ReadFile(gi); string(b) != old {
		t.Fatal("a check without --fix must not touch the file")
	}

	out = captureStdout(t, func() { problems = checkManagedBlock(gi, true) })
	if problems != 0 || !strings.Contains(out, "Updated") {
		t.Fatalf("stale block with --fix: problems=%d out=%q", problems, out)
	}
	b, _ := os.ReadFile(gi)
	got, found := managedBlockRules(string(b))
	if !found || strings.Join(got, ",") != strings.Join(unignoreRules(), ",") {
		t.Errorf("rewritten rules = %v (found=%v)", got, found)
	}
	for _, keep := range []string{"*.env\n**/data/**\n", "# hand-written, after the block\n!pi/keep/me\n"} {
		if !strings.Contains(string(b), keep) {
			t.Errorf("content outside the block was lost (%q):\n%s", keep, b)
		}
	}
	// A second run finds nothing to do, and the file does not change (idempotent).
	out = captureStdout(t, func() { problems = checkManagedBlock(gi, true) })
	b2, _ := os.ReadFile(gi)
	if problems != 0 || string(b2) != string(b) || !strings.Contains(out, "current") {
		t.Errorf("second run: problems=%d changed=%v out=%q", problems, string(b2) != string(b), out)
	}
}

// Blanket rules outside hemma's block (the predecessor tool's) are advised about, never edited, and
// do not count as a problem; a repo with no hemma block is left without one.
func TestCheckManagedBlock_StrayBlanketRulesAreAdvisoryOnly(t *testing.T) {
	stray := "**/data/**\n# >>> shd managed >>>\n!**/data/\n!**/data/**/\n# <<< shd managed <<<\n"
	gi := filepath.Join(t.TempDir(), ".gitignore")
	if err := os.WriteFile(gi, []byte(stray), 0o644); err != nil {
		t.Fatal(err)
	}
	var problems int
	out := captureStdout(t, func() { problems = checkManagedBlock(gi, true) })
	if problems != 0 {
		t.Errorf("an advisory must not count as a problem, got %d", problems)
	}
	if !strings.Contains(out, "blanket data/ rules") || !strings.Contains(out, "!**/data/") {
		t.Errorf("expected the advisory naming the rules, got %q", out)
	}
	if b, _ := os.ReadFile(gi); string(b) != stray {
		t.Errorf("hemma must not edit rules it does not own:\n%s", b)
	}
	// No file at all: nothing to check.
	if n := checkManagedBlock(filepath.Join(t.TempDir(), "absent"), true); n != 0 {
		t.Errorf("missing .gitignore: problems = %d", n)
	}
}
