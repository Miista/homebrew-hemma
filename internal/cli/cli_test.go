package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkdirs creates host directories inside the temp repo so `host add` (which
// requires the host name to be an existing directory) succeeds.
func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// seed writes a minimal valid services.yaml into dir.
func seed(t *testing.T, dir string) {
	t.Helper()
	content := `hosts:
  resolver: {ip: 192.0.2.1, dir: resolver}
  appbox: {ip: 192.0.2.2, dir: appbox}
domains:
  - example.com
defaults:
  dns_host: resolver
services: {}
`
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun_ApplyMissingConfig(t *testing.T) {
	dir := t.TempDir()
	if code := Run([]string{"-C", dir, "apply"}); code != 1 {
		t.Errorf("apply on missing services.yaml should exit 1, got %d", code)
	}
}

func TestRun_UpdateAndRemoveMissingConfig(t *testing.T) {
	dir := t.TempDir()
	if code := Run([]string{"-C", dir, "update", "service", "foo", "--backend", "a:1"}); code != 1 {
		t.Errorf("update on missing config should exit 1, got %d", code)
	}
	if code := Run([]string{"-C", dir, "remove", "service", "foo"}); code != 1 {
		t.Errorf("remove on missing config should exit 1, got %d", code)
	}
}

func TestRun_AddCreatesConfig(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})
	if code != 0 {
		t.Fatalf("valid add should exit 0, got %d", code)
	}
	// Generated outputs exist.
	if _, err := os.Stat(filepath.Join(dir, "appbox", "caddy/data/sites/docs.caddy")); err != nil {
		t.Errorf("caddy site not generated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "resolver", "pihole/data/dnsmasq.d/docs.generated.conf")); err != nil {
		t.Errorf("dns conf not generated: %v", err)
	}
}

func TestRun_AddServiceRefusesUnknownFQDN(t *testing.T) {
	// add must refuse a service whose fqdn matches no defined domain (catches
	// typos like .dl for .dk) and NOT persist it.
	dir := t.TempDir()
	seed(t, dir) // defines example.com + hosts
	code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.dl", "--host", "appbox", "--backend", "paperless:8000"})
	if code != 1 {
		t.Errorf("add with unmatched fqdn should exit 1, got %d", code)
	}
	if _, ok := load(t, dir).Services["docs"]; ok {
		t.Error("service with unmatched fqdn must not be persisted")
	}
}

func TestRun_AddServiceRefusesUnknownHost(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "ghost", "--backend", "paperless:8000"})
	if code != 1 {
		t.Errorf("add with unknown host should exit 1, got %d", code)
	}
	if _, ok := load(t, dir).Services["docs"]; ok {
		t.Error("service with unknown host must not be persisted")
	}
}

func TestRun_AddDuplicateFails(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	args := []string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}
	if code := Run(args); code != 0 {
		t.Fatalf("first add should succeed, got %d", code)
	}
	if code := Run(args); code != 1 {
		t.Errorf("duplicate add should exit 1, got %d", code)
	}
}

func TestRun_SyncReportsSkips(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	mkdirs(t, dir, "resolver", "appbox")
	// Valid fqdn/host/domain (passes add) but a malformed backend — the planner
	// skips it during add's sync tail -> exit 1. (add doesn't validate backend
	// shape, so the skip surfaces at reconcile time.)
	code := Run([]string{"-C", dir, "add", "service", "x",
		"--fqdn", "x.example.com", "--host", "resolver", "--backend", "noport"})
	if code != 1 {
		t.Errorf("add with a skipped entry should exit 1, got %d", code)
	}
}

// --- validation guards added in the UX pass ---

func TestRun_AddMissingFlagsDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	if code := Run([]string{"-C", dir, "add", "service", "docs"}); code != 2 {
		t.Errorf("add with no flags should exit 2, got %d", code)
	}
	// Crucially: no services.yaml should have been written.
	if _, err := os.Stat(filepath.Join(dir, configName)); !os.IsNotExist(err) {
		t.Error("add with missing flags must not create services.yaml")
	}
}

func TestRun_AddPartialFlagsRejected(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com"}); code != 2 {
		t.Errorf("add missing --host/--backend should exit 2, got %d", code)
	}
	if _, ok := load(t, dir).Services["docs"]; ok {
		t.Error("partial add must not persist the service")
	}
}

func TestRun_UpdateNoOpRejected(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})
	if code := Run([]string{"-C", dir, "update", "docs"}); code != 2 {
		t.Errorf("update with no field flags should exit 2, got %d", code)
	}
}

func TestRun_List(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir) // resolver + appbox, dns_host resolver, example.com
	mkdirs(t, dir, "resolver", "appbox")
	Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})

	// list is plain inventory — always exits 0, regardless of service validity.
	if code := Run([]string{"-C", dir, "list"}); code != 0 {
		t.Errorf("list should exit 0 (plain inventory), got %d", code)
	}
}

func TestRun_ListMissingConfig(t *testing.T) {
	dir := t.TempDir()
	if code := Run([]string{"-C", dir, "list"}); code != 1 {
		t.Errorf("list with no services.yaml should exit 1, got %d", code)
	}
}

// End-to-end: set auth-snippet, opt a service in, and verify the generated
// (auth) file, the site import, and that a non-auth service stays clean.
func TestRun_AuthSnippetFlow(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "snip.caddy"),
		[]byte("forward_auth https://auth.example.com {\n\turi /api/authz/forward-auth\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"}); code != 0 {
		t.Fatalf("set auth-snippet should exit 0, got %d", code)
	}
	// The (auth) file is generated on every host with the source body.
	for _, host := range []string{"resolver", "appbox"} {
		b, err := os.ReadFile(filepath.Join(dir, host, "caddy/data/splitdns.auth.generated.caddy"))
		if err != nil {
			t.Fatalf("%s: auth snippet not generated: %v", host, err)
		}
		if !contains(string(b), "forward_auth https://auth.example.com") {
			t.Errorf("%s: auth body missing: %s", host, b)
		}
	}

	// A service opting in imports auth; one that doesn't, doesn't.
	Run([]string{"-C", dir, "add", "service", "gatus", "--fqdn", "status.example.com", "--host", "appbox", "--backend", "gatus:8080", "--auth"})
	Run([]string{"-C", dir, "add", "service", "open", "--fqdn", "open.example.com", "--host", "appbox", "--backend", "open:3000"})

	gatus, _ := os.ReadFile(filepath.Join(dir, "appbox", "caddy/data/sites/gatus.caddy"))
	if !contains(string(gatus), "\timport auth\n") {
		t.Errorf("gatus should import auth: %s", gatus)
	}
	open, _ := os.ReadFile(filepath.Join(dir, "appbox", "caddy/data/sites/open.caddy"))
	if contains(string(open), "import auth") {
		t.Errorf("open should NOT import auth: %s", open)
	}
}

// Clearing the auth-snippet resets the generated file to the empty stub.
func TestRun_SetAuthSnippetClear(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth x { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})

	if code := Run([]string{"-C", dir, "set", "auth-snippet", "-"}); code != 0 {
		t.Fatalf("clear should exit 0, got %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "resolver", "caddy/data/splitdns.auth.generated.caddy"))
	if contains(string(b), "forward_auth") {
		t.Errorf("cleared snippet should be empty stub, got: %s", b)
	}
	if !contains(string(b), "(auth) {\n}") {
		t.Errorf("expected empty (auth) stub, got: %s", b)
	}
}

// A nonexistent auth-snippet path is rejected at set time (before persisting).
func TestRun_SetAuthSnippetRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "set", "auth-snippet", "nope.caddy"}); code != 1 {
		t.Errorf("missing source should exit 1, got %d", code)
	}
	// Nothing should have been persisted.
	cfg, _ := os.ReadFile(filepath.Join(dir, configName))
	if contains(string(cfg), "auth_snippet") {
		t.Errorf("rejected path must not persist: %s", cfg)
	}
}

// doctor flags source-vs-generated drift when the snippet source changes
// without a re-sync.
func TestRun_DoctorDetectsAuthDrift(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v1 { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})

	// Clean right after sync.
	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Fatalf("doctor should be clean after sync, got %d", code)
	}
	// Edit source WITHOUT syncing → drift.
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v2 { }\n"), 0o644)
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Errorf("doctor should detect drift after source edit, got exit 0")
	}
}

// doctor runs its FULL audit (gitignore + Caddyfile + drift) cleanly right
// after `set auth-snippet`, and flags drift once the source is edited without a
// re-sync. Running inside a real git repo exercises the gitignore-check plan,
// which cmdDoctor must build from the loaded auth snippet (not the empty stub).
func TestRun_DoctorAuthSnippetCleanThenDrift(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	// Un-ignore generated files so the gitignore check passes and doctor can
	// reach a fully-clean state after sync.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
		[]byte("**/data/**\n"+strings.Join(unignoreRules(), "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v1 { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})

	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Fatalf("doctor should be clean after set auth-snippet, got %d", code)
	}
	// Edit the source without a re-sync → the generated (auth) file is stale.
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v2 { }\n"), 0o644)
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Errorf("doctor should flag drift after source edit, got exit 0")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
