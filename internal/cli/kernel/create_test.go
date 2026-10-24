package kernel

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// seedWithAuth writes a services.yaml with an auth service and one OIDC
// service that has groups, for the create/doctor tests.
func seedWithAuth(t *testing.T, dir string) {
	t.Helper()
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
  grafana:
    fqdn: grafana.example.com
    host: appbox
    backend: grafana:3000
    auth:
      mode: oidc
      groups: [admins]
  docsapp:
    fqdn: docs.example.com
    host: appbox
    backend: docsapp:8000
    auth:
      mode: oidc
`
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAppOIDC_UsesServiceFQDNAndNamedPolicy(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "create", "app", "oidc", "grafana"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	// grafana is a configured service with groups: real fqdn + named policy.
	if !strings.Contains(out, "- 'https://grafana.example.com/CHANGEME'") {
		t.Errorf("redirect_uri should use the service fqdn and /CHANGEME default:\n%s", out)
	}
	if !strings.Contains(out, "authorization_policy: 'grafana'") {
		t.Errorf("groups should select the generated named policy:\n%s", out)
	}
	if !strings.Contains(out, "Created credentials for client grafana") ||
		!strings.Contains(out, "Client Secret (grafana): ") ||
		!strings.Contains(out, "Client Secret (Authelia): $pbkdf2-sha512$310000$") {
		t.Errorf("authcli-compatible header lines missing:\n%s", out)
	}
	// id and secret are 72 chars of the rfc3986 unreserved charset.
	for _, label := range []string{"Client ID: ", "Client Secret (grafana): "} {
		re := regexp.MustCompile(regexp.QuoteMeta(label) + `([A-Za-z0-9._~-]+)\n`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("missing %q line:\n%s", label, out)
		}
		if len(m[1]) != 72 {
			t.Errorf("%s length = %d, want 72", label, len(m[1]))
		}
	}
}

func TestCreateAppOIDC_ServiceWithoutGroupsKeepsOneFactor(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	out := captureStdout(t, func() {
		Run([]string{"-C", dir, "create", "app", "oidc", "docsapp", "/accounts/oidc/authelia/login/callback/"})
	})
	if !strings.Contains(out, "authorization_policy: 'one_factor'") {
		t.Errorf("service without groups should keep one_factor:\n%s", out)
	}
	if !strings.Contains(out, "- 'https://docs.example.com/accounts/oidc/authelia/login/callback/'") {
		t.Errorf("explicit callback_path should be appended to the service fqdn:\n%s", out)
	}
}

func TestCreateAppOIDC_UnknownAppDerivesFromDomains(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir) // domains: [example.com]
	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "create", "app", "oidc", "myapp"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "- 'https://myapp.example.com/CHANGEME'") {
		t.Errorf("unknown app should derive <app>.<first-domain>:\n%s", out)
	}
	if !strings.Contains(out, "authorization_policy: 'one_factor'") {
		t.Errorf("unknown app should default to one_factor:\n%s", out)
	}
}

func TestCreateAppOIDC_UnknownAppNoDomainsRefuses(t *testing.T) {
	dir := t.TempDir() // no services.yaml at all → no domains to derive from
	if code := Run([]string{"-C", dir, "create", "app", "oidc", "myapp"}); code != 1 {
		t.Errorf("unknown app with no configured domains should exit 1, got %d", code)
	}
}

func TestCreateAppOIDC_Usage(t *testing.T) {
	if code := Run([]string{"create", "app", "oidc"}); code != 2 {
		t.Errorf("missing app name should exit 2, got %d", code)
	}
	if code := Run([]string{"create", "app", "saml", "x"}); code != 2 {
		t.Errorf("unknown app type should exit 2, got %d", code)
	}
	if code := Run([]string{"create"}); code != 2 {
		t.Errorf("bare create should exit 2, got %d", code)
	}
	if code := Run([]string{"create", "widget"}); code != 2 {
		t.Errorf("unknown create noun should exit 2, got %d", code)
	}
}

func TestCreateUser_RefusesNonTTY(t *testing.T) {
	// Under `go test`, stdin is not a terminal — the command must refuse
	// rather than hang or echo a password.
	if code := Run([]string{"create", "user", "alice"}); code != 1 {
		t.Errorf("create user without a TTY should exit 1, got %d", code)
	}
	if code := Run([]string{"create", "user"}); code != 2 {
		t.Errorf("create user without a username should exit 2, got %d", code)
	}
}

// writeAutheliaFixture writes the provider config + users db under the auth
// service's host dir, mirroring the conventional layout.
func writeAutheliaFixture(t *testing.T, dir, usersYAML string) {
	t.Helper()
	cfgDir := filepath.Join(dir, "appbox", "authelia", "data", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "configuration.yml"), []byte("authentication_backend:\n  file:\n    path: /config/users_database.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if usersYAML != "" {
		if err := os.WriteFile(filepath.Join(cfgDir, "users_database.yml"), []byte(usersYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsersDBWarnings_WiredThroughRepoLayout(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAutheliaFixture(t, dir, "users:\n  alice:\n    groups: [editors]\n")

	cfg, code := loadExisting(stdEnv(), filepath.Join(dir, configName), "test")
	if cfg == nil {
		t.Fatalf("load: %d", code)
	}
	w := usersDBWarnings(dir, cfg)
	var parts []string
	for _, a := range w {
		parts = append(parts, a.String())
	}
	joined := strings.Join(parts, "\n")
	// grafana's group "admins" is on no user: typo + nobody-can-access.
	if len(w) != 2 || !strings.Contains(joined, `"admins" (service grafana)`) || !strings.Contains(joined, "nobody can access") {
		t.Errorf("want admins typo + unreachable advisories, got %v", w)
	}
}

func TestUsersDBWarnings_GatedOnFileExisting(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAutheliaFixture(t, dir, "") // config present, users db absent

	cfg, _ := loadExisting(stdEnv(), filepath.Join(dir, configName), "test")
	if w := usersDBWarnings(dir, cfg); w != nil {
		t.Errorf("no users db -> check must be silent, got %v", w)
	}
}

func TestDoctor_ReportsUsersDBWarnings(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	mkdirs(t, dir, "appbox")
	writeAutheliaFixture(t, dir, "users:\n  alice:\n    groups: [editors]\n")
	// Bring generated files in sync so drift doesn't dominate the output.
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })

	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if !strings.Contains(out, `auth group "admins" (service grafana)`) {
		t.Errorf("doctor should surface users-db group warnings:\n%s", out)
	}
	if strings.Contains(out, "alice") || strings.Contains(out, "@") {
		t.Errorf("doctor warnings must not leak usernames/emails:\n%s", out)
	}
}
