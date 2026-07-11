package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/auth"
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
		b, err := os.ReadFile(filepath.Join(dir, host, "caddy/data/hemma.auth.generated.caddy"))
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
	b, _ := os.ReadFile(filepath.Join(dir, "resolver", "caddy/data/hemma.auth.generated.caddy"))
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
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v1 { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})

	// Clean right after sync (snippet + service both set → no config warning).
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
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v1 { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})

	if code := Run([]string{"-C", dir, "doctor"}); code != 0 {
		t.Fatalf("doctor should be clean after set auth-snippet, got %d", code)
	}
	// Edit the source without a re-sync → the generated (auth) file is stale.
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth v2 { }\n"), 0o644)
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Errorf("doctor should flag drift after source edit, got exit 0")
	}
}

// set auth-service names the auth backend; its block preserves X-Forwarded-Host.
func TestRun_SetAuthServiceRendersHeaderPreserve(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	if code := Run([]string{"-C", dir, "set", "auth-service", "authelia"}); code != 0 {
		t.Fatalf("set auth-service should exit 0, got %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "appbox", "caddy/data/sites/authelia.caddy"))
	if !contains(string(b), "header_up X-Forwarded-Host {header.X-Forwarded-Host}") {
		t.Errorf("auth_service block should preserve X-Forwarded-Host:\n%s", b)
	}
}

// set auth-service refuses a service that doesn't exist and doesn't persist it.
func TestRun_SetAuthServiceRejectsUnknown(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "set", "auth-service", "ghost"}); code != 1 {
		t.Errorf("unknown service should exit 1, got %d", code)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, configName))
	if contains(string(cfg), "auth_service") {
		t.Errorf("rejected auth_service must not persist: %s", cfg)
	}
}

// doctor is non-zero (footgun) when auth_snippet is set but auth_service isn't.
func TestRun_DoctorWarnsSnippetWithoutService(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	os.WriteFile(filepath.Join(dir, "snip.caddy"), []byte("forward_auth x { }\n"), 0o644)
	Run([]string{"-C", dir, "set", "auth-snippet", "snip.caddy"})

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if !contains(out, "auth_service is not") {
		t.Errorf("doctor should warn about missing auth_service:\n%s", out)
	}
	if code := Run([]string{"-C", dir, "doctor"}); code == 0 {
		t.Errorf("doctor should exit non-zero for the snippet-without-service footgun")
	}
}

// Clearing auth-service ('-') removes the header-preserve from the block.
func TestRun_SetAuthServiceClear(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})

	if code := Run([]string{"-C", dir, "set", "auth-service", "-"}); code != 0 {
		t.Fatalf("clear should exit 0, got %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "appbox", "caddy/data/sites/authelia.caddy"))
	if contains(string(b), "X-Forwarded-Host") {
		t.Errorf("cleared auth_service block should not preserve X-Forwarded-Host:\n%s", b)
	}
}

// authConfigWarnings covers the reverse (service without snippet) and the
// unused-auth note, exercised through `list` output.
func TestRun_AuthConfigWarnings(t *testing.T) {
	// service set, snippet unset → "auth_service is set but auth_snippet is not".
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !contains(out, "auth_snippet is not") {
		t.Errorf("expected reverse warning (service without snippet):\n%s", out)
	}

	// both set but no service opted in → the unused-auth note.
	dir2 := t.TempDir()
	mkdirs(t, dir2, "resolver", "appbox")
	seed(t, dir2)
	os.WriteFile(filepath.Join(dir2, "snip.caddy"), []byte("forward_auth x { }\n"), 0o644)
	Run([]string{"-C", dir2, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir2, "set", "auth-snippet", "snip.caddy"})
	Run([]string{"-C", dir2, "set", "auth-service", "authelia"})
	out2 := captureStdout(t, func() { Run([]string{"-C", dir2, "list", "--all"}) })
	if !contains(out2, "no service uses forward auth") {
		t.Errorf("expected unused-auth note when nothing opted in:\n%s", out2)
	}
}

// An auth: oidc service with no matching Authelia OIDC client warns; adding the
// client (a redirect_uri on https://<fqdn>/) clears it.
func TestRun_OIDCClientWarning(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	// The auth_service (authelia) runs on appbox; its config lives under appbox.
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})
	Run([]string{"-C", dir, "add", "service", "app", "--fqdn", "app.example.com", "--host", "appbox", "--backend", "app:3000", "--auth-mode", "oidc"})

	// No Authelia config yet → soft advisory ("could not verify").
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !contains(out, "could not verify OIDC client for app") {
		t.Errorf("expected soft advisory when Authelia config missing:\n%s", out)
	}

	// Authelia config present but no client for app → hard warning.
	acfg := filepath.Join(dir, "appbox", auth.Default().ConfigPath())
	os.MkdirAll(filepath.Dir(acfg), 0o755)
	os.WriteFile(acfg, []byte("identity_providers:\n  oidc:\n    clients:\n      - client_id: other\n        redirect_uris:\n          - https://other.example.com/accounts/oidc/callback\n"), 0o644)
	out2 := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !contains(out2, "no Authelia OIDC client registers a redirect_uri for https://app.example.com/") {
		t.Errorf("expected unregistered-client warning:\n%s", out2)
	}

	// Add the matching client → warning clears.
	os.WriteFile(acfg, []byte("identity_providers:\n  oidc:\n    clients:\n      - client_id: app\n        redirect_uris:\n          - https://app.example.com/accounts/oidc/callback\n"), 0o644)
	out3 := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if contains(out3, "auth: oidc but no Authelia") || contains(out3, "could not verify") {
		t.Errorf("warning should clear once the client is registered:\n%s", out3)
	}
}

// --auth-mode validates its value; an invalid mode is a usage error (exit 2).
func TestRun_AuthModeInvalid(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	code := Run([]string{"-C", dir, "add", "service", "x", "--fqdn", "x.example.com", "--host", "appbox", "--backend", "x:1", "--auth-mode", "bogus"})
	if code != 2 {
		t.Errorf("invalid --auth-mode should exit 2, got %d", code)
	}
}

// --auth back-compat: shorthand for forward, persisted as `auth: forward`.
func TestRun_AuthShorthandForward(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "x", "--fqdn", "x.example.com", "--host", "appbox", "--backend", "x:1", "--auth"}); code != 0 {
		t.Fatalf("add --auth exit %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, configName))
	if !contains(string(b), "auth: forward") {
		t.Errorf("--auth should persist as forward:\n%s", b)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// --auth-groups sets groups on add/update (persisting the object YAML form),
// clears with ”, and is refused without an auth mode (validate-before-persist)
// — plus the generated access-control artifact lands on the auth host.
func TestRun_AuthGroups(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})

	// Groups without a mode: usage error, nothing persisted.
	if code := Run([]string{"-C", dir, "add", "service", "bad", "--fqdn", "bad.example.com", "--host", "appbox", "--backend", "b:1", "--auth-groups", "admins"}); code != 2 {
		t.Errorf("groups without mode should be a usage error, got %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, configName)); strings.Contains(string(b), "bad.example.com") {
		t.Error("refused add must not persist")
	}

	if code := Run([]string{"-C", dir, "add", "service", "pihole", "--fqdn", "pihole.example.com", "--host", "appbox", "--backend", "pihole:80", "--auth-mode", "forward", "--auth-groups", "admins, family"}); code != 0 {
		t.Fatalf("add with groups failed: %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, configName))
	if !strings.Contains(string(b), "mode: forward") || !strings.Contains(string(b), "- admins") || !strings.Contains(string(b), "- family") {
		t.Errorf("groups should persist in object form:\n%s", b)
	}

	// The access-control artifact is generated on the auth host.
	ac, err := os.ReadFile(filepath.Join(dir, "appbox", "authelia/data/config/hemma.access_control.generated.yml"))
	if err != nil {
		t.Fatalf("access-control artifact missing: %v", err)
	}
	if !strings.Contains(string(ac), "['group:admins']") || !strings.Contains(string(ac), "['group:family']") {
		t.Errorf("artifact should OR the groups:\n%s", ac)
	}

	// Clearing the mode while groups remain is refused before persisting.
	if code := Run([]string{"-C", dir, "update", "service", "pihole", "--auth-mode", "none"}); code != 2 {
		t.Errorf("mode none with lingering groups should be refused, got %d", code)
	}

	// Clearing groups collapses back to the short YAML form.
	if code := Run([]string{"-C", dir, "update", "service", "pihole", "--auth-groups", ""}); code != 0 {
		t.Fatalf("clearing groups failed: %d", code)
	}
	b, _ = os.ReadFile(filepath.Join(dir, configName))
	if !strings.Contains(string(b), "auth: forward") || strings.Contains(string(b), "- admins") {
		t.Errorf("cleared groups should re-emit the short form:\n%s", b)
	}
}

// An oidc service with groups warns until its Authelia client references the
// generated authorization_policy (surfaced through the normal warning path).
func TestRun_OIDCAuthorizationPolicyWarning(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "authelia", "--fqdn", "auth.example.com", "--host", "appbox", "--backend", "authelia:9091"})
	Run([]string{"-C", dir, "set", "auth-service", "authelia"})

	acfg := filepath.Join(dir, "appbox", auth.Default().ConfigPath())
	os.MkdirAll(filepath.Dir(acfg), 0o755)
	os.WriteFile(acfg, []byte("identity_providers:\n  oidc:\n    clients:\n      - client_id: app\n        redirect_uris:\n          - https://app.example.com/accounts/oidc/callback\n"), 0o644)

	out := captureStdout(t, func() {
		Run([]string{"-C", dir, "add", "service", "app", "--fqdn", "app.example.com", "--host", "appbox", "--backend", "app:3000", "--auth-mode", "oidc", "--auth-groups", "users"})
	})
	if !contains(out, "authorization_policy: 'app'") {
		t.Errorf("expected authorization_policy warning:\n%s", out)
	}

	// Client references the policy → warning clears.
	os.WriteFile(acfg, []byte("identity_providers:\n  oidc:\n    clients:\n      - client_id: app\n        authorization_policy: app\n        redirect_uris:\n          - https://app.example.com/accounts/oidc/callback\n"), 0o644)
	out2 := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if contains(out2, "authorization_policy") {
		t.Errorf("warning should clear once the client references the policy:\n%s", out2)
	}
}
