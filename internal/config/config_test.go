package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_Missing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "services.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c.Exists {
		t.Error("Exists should be false for a missing file")
	}
	if len(c.Services) != 0 {
		t.Errorf("missing file should yield empty services, got %d", len(c.Services))
	}
}

func TestLoad_Present(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "machines: {}\ndomains: []\nservices: {}\n")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Exists {
		t.Error("Exists should be true for a present file")
	}
}

func TestLoad_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "machines: {\n  this is : : not yaml\n")

	if _, err := Load(path); err == nil {
		t.Fatal("malformed yaml should be a fatal error")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")

	c, _ := Load(path)
	c.Hosts["resolver"] = Host{IP: "192.0.2.1", Dir: "resolver"}
	c.Domains["example.com"] = Domain{}
	c.Defaults.DNSHost = "resolver"
	c.Services["docs"] = Service{FQDN: "docs.example.com", Host: "resolver", Backend: "docsapp:8000"}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c2.Hosts["resolver"].IP != "192.0.2.1" {
		t.Errorf("host not round-tripped: %+v", c2.Hosts["resolver"])
	}
	if c2.Services["docs"].Backend != "docsapp:8000" {
		t.Errorf("service not round-tripped: %+v", c2.Services["docs"])
	}
	if c2.Defaults.DNSHost != "resolver" {
		t.Errorf("defaults not round-tripped: %+v", c2.Defaults)
	}
}

func TestResolvedDir(t *testing.T) {
	// Empty Dir defaults to the host name; an explicit Dir overrides it.
	if got := (Host{}).ResolvedDir("resolver"); got != "resolver" {
		t.Errorf("empty Dir should default to name, got %q", got)
	}
	if got := (Host{Dir: "custom"}).ResolvedDir("resolver"); got != "custom" {
		t.Errorf("explicit Dir should win, got %q", got)
	}
}

func TestDNSHost(t *testing.T) {
	c := &Config{Defaults: Defaults{DNSHost: "resolver"}}
	if got := c.DNSHost(); got != "resolver" {
		t.Errorf("dns_host: got %q want resolver", got)
	}
}

// AtomicWrite must not leave temp files behind on success (design §9).
func TestAtomicWrite_NoTempLeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.conf")
	if err := AtomicWrite(path, []byte("hi")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hi" {
		t.Errorf("content: got %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "out.conf" {
			t.Errorf("stray file left behind: %q", e.Name())
		}
	}
}

// AtomicWrite must produce a world-readable file (0644), not os.CreateTemp's
// default 0600 — real bug, hit in production twice: cloudflared's config.yml
// is read by its distroless image's UID 65532, which never matches whatever
// user runs hemma, so a 0600 file silently crash-loops the container on the
// very next sync that regenerates it.
func TestAtomicWrite_ProducesWorldReadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := AtomicWrite(path, []byte("ingress: []\n")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("AtomicWrite produced mode %o, want 0644 (readable by a container running as a different UID than the writer)", perm)
	}
}

func TestServicesUsingHost(t *testing.T) {
	c := &Config{
		Defaults: Defaults{DNSHost: "resolver"},
		Services: map[string]Service{
			"docs":   {FQDN: "docs.example.com", Host: "appbox"},
			"photos": {FQDN: "photos.example.net", Host: "resolver"},
			"vault":  {FQDN: "vault.example.net", Host: "appbox"},
		},
	}
	// appbox runs docs+vault.
	if got := c.ServicesUsingHost("appbox"); len(got) != 2 || got[0] != "docs" || got[1] != "vault" {
		t.Errorf("appbox users: %v", got)
	}
	// resolver is the dns_host — EVERY service depends on it (plus it runs photos).
	got := c.ServicesUsingHost("resolver")
	if len(got) != 3 {
		t.Errorf("resolver (the dns_host) should be used by all 3 services, got %v", got)
	}
	if got := c.ServicesUsingHost("ghost"); len(got) != 0 {
		t.Errorf("unused host should have no users: %v", got)
	}
}

func TestServicesUsingDomain(t *testing.T) {
	c := &Config{Services: map[string]Service{
		"docs":   {FQDN: "docs.example.com"},
		"sub":    {FQDN: "a.b.example.com"},
		"photos": {FQDN: "photos.example.net"},
	}}
	got := c.ServicesUsingDomain("example.com")
	if len(got) != 2 || got[0] != "docs" || got[1] != "sub" {
		t.Errorf("example.com users: %v", got)
	}
	if got := c.ServicesUsingDomain("unused.net"); len(got) != 0 {
		t.Errorf("unused domain should have no users: %v", got)
	}
}

// Auth mode parsing: legacy bool (true→forward, false→none), string forms
// (forward/oidc/none), absent (none), and invalid (none + error).
func TestAuthMode_Parse(t *testing.T) {
	load := func(t *testing.T, authLine string) (Service, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "services.yaml")
		body := "hosts: {}\ndomains: []\ndefaults: {}\nservices:\n  s:\n    fqdn: s.example.com\n    host: h\n    backend: x:1\n"
		if authLine != "" {
			body += "    auth: " + authLine + "\n"
		}
		write(t, path, body)
		c, err := Load(path)
		if err != nil {
			return Service{}, err
		}
		return c.Services["s"], nil
	}
	cases := []struct {
		name, line string
		want       AuthMode
		wantErr    bool
	}{
		{"legacy true", "true", AuthForward, false},
		{"legacy false", "false", AuthNone, false},
		{"absent", "", AuthNone, false},
		{"forward", "forward", AuthForward, false},
		{"oidc", "oidc", AuthOIDC, false},
		{"none string", "none", AuthNone, false},
		{"invalid", "bogus", AuthNone, true}, // documented: invalid → none + error
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := load(t, tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected parse error for %q", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if svc.Auth.Mode != tc.want {
				t.Errorf("auth %q → %q, want %q", tc.line, svc.Auth.Mode, tc.want)
			}
		})
	}
}

// A legacy `auth: true` must READ as forward and, on Save, re-emit the string
// form (auth: forward), while none is omitted entirely.
func TestAuthMode_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "hosts: {}\ndomains: []\ndefaults: {}\nservices:\n  legacy:\n    fqdn: a.example.com\n    host: h\n    backend: x:1\n    auth: true\n  plain:\n    fqdn: b.example.com\n    host: h\n    backend: x:1\n  odc:\n    fqdn: c.example.com\n    host: h\n    backend: x:1\n    auth: oidc\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Services["legacy"].Auth.Mode != AuthForward {
		t.Errorf("legacy true should read as forward, got %q", c.Services["legacy"].Auth.Mode)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.Contains(s, "auth: forward") {
		t.Errorf("legacy true should re-emit as 'auth: forward':\n%s", s)
	}
	if !strings.Contains(s, "auth: oidc") {
		t.Errorf("oidc should serialize as 'auth: oidc':\n%s", s)
	}
	// none is omitted (omitempty).
	if strings.Contains(s, "auth: none") || strings.Contains(s, "auth: false") {
		t.Errorf("none auth should be omitted, not emitted:\n%s", s)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAuthSnippet_Unset(t *testing.T) {
	c := &Config{AuthSnippetBody: "stale"}
	if err := c.LoadAuthSnippet(t.TempDir()); err != nil {
		t.Fatalf("unset should not error: %v", err)
	}
	if c.AuthSnippetBody != "" {
		t.Errorf("unset auth_snippet should clear body, got %q", c.AuthSnippetBody)
	}
}

func TestLoadAuthSnippet_RelativeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "snip.caddy"), "forward_auth x { }")

	// relative path is resolved against repoRoot
	c := &Config{Defaults: Defaults{AuthSnippet: "snip.caddy"}}
	if err := c.LoadAuthSnippet(dir); err != nil {
		t.Fatalf("relative: %v", err)
	}
	if c.AuthSnippetBody != "forward_auth x { }" {
		t.Errorf("relative body wrong: %q", c.AuthSnippetBody)
	}

	// absolute path is used as-is
	c2 := &Config{Defaults: Defaults{AuthSnippet: filepath.Join(dir, "snip.caddy")}}
	if err := c2.LoadAuthSnippet("/nonexistent-root"); err != nil {
		t.Fatalf("absolute: %v", err)
	}
	if c2.AuthSnippetBody != "forward_auth x { }" {
		t.Errorf("absolute body wrong: %q", c2.AuthSnippetBody)
	}
}

// A missing source must return an error WITHOUT clearing a previously-loaded
// body — the keep-last-good invariant that stops a typo disabling auth.
func TestLoadAuthSnippet_MissingKeepsBody(t *testing.T) {
	c := &Config{
		Defaults:        Defaults{AuthSnippet: "gone.caddy"},
		AuthSnippetBody: "last-good-body",
	}
	err := c.LoadAuthSnippet(t.TempDir())
	if err == nil {
		t.Fatal("missing source should error")
	}
	if c.AuthSnippetBody != "last-good-body" {
		t.Errorf("body must be preserved on error, got %q", c.AuthSnippetBody)
	}
}

// The object form of auth ({mode, groups}) parses; groups round-trip in the
// object form while a groupless mode re-emits the SHORT string form.
func TestAuth_ObjectForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  gated:
    fqdn: a.example.com
    host: h
    backend: x:1
    auth:
      mode: forward
      groups: [admins, family]
  short:
    fqdn: b.example.com
    host: h
    backend: x:1
    auth:
      mode: oidc
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Services["gated"].Auth
	if g.Mode != AuthForward || len(g.Groups) != 2 || g.Groups[0] != "admins" || g.Groups[1] != "family" {
		t.Fatalf("gated parsed wrong: %+v", g)
	}
	if s := c.Services["short"].Auth; s.Mode != AuthOIDC || len(s.Groups) != 0 {
		t.Fatalf("short parsed wrong: %+v", s)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	// Groups set → object form survives the round trip.
	if !strings.Contains(out, "mode: forward") || !strings.Contains(out, "- admins") {
		t.Errorf("groups should round-trip in object form:\n%s", out)
	}
	// No groups → the terse string form, not a one-key mapping.
	if !strings.Contains(out, "auth: oidc") {
		t.Errorf("groupless mode should re-emit the short form:\n%s", out)
	}
}

// Object form without a mode is a parse error (mode is required there), and an
// unknown mode errors just like the string form.
func TestAuth_ObjectFormErrors(t *testing.T) {
	load := func(body string) error {
		dir := t.TempDir()
		path := filepath.Join(dir, "services.yaml")
		write(t, path, "hosts: {}\ndomains: []\ndefaults: {}\nservices:\n  s:\n    fqdn: s.example.com\n    host: h\n    backend: x:1\n    auth:\n"+body)
		_, err := Load(path)
		return err
	}
	if err := load("      groups: [admins]\n"); err == nil {
		t.Error("object form without mode should error")
	}
	if err := load("      mode: bogus\n"); err == nil {
		t.Error("object form with unknown mode should error")
	}
}

// The optional ssh: field on a host round-trips through save/load, is dropped
// from the YAML when empty (omitempty), and SSHDest defaults to the host name.
func TestHostSSH_RoundTripAndDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "hosts:\n  edge: {ip: 192.0.2.1}\ndomains: []\ndefaults: {}\nservices: {}\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Default: no ssh field → destination is the host name.
	if got := c.Hosts["edge"].SSHDest("edge"); got != "edge" {
		t.Errorf("SSHDest default should be the host name, got %q", got)
	}
	// Set a verbatim destination and round-trip it.
	c.Hosts["edge"] = Host{IP: "192.0.2.1", SSH: "admin@raspberry.local"}
	c.Hosts["box"] = Host{IP: "192.0.2.2"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Hosts["edge"].SSH; got != "admin@raspberry.local" {
		t.Errorf("ssh field should round-trip, got %q", got)
	}
	if got := c2.Hosts["edge"].SSHDest("edge"); got != "admin@raspberry.local" {
		t.Errorf("SSHDest should return the explicit destination, got %q", got)
	}
	// A host without ssh must not gain the key on disk (omitempty).
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "box: {ip: 192.0.2.2, ssh") || strings.Count(string(b), "ssh:") != 1 {
		t.Errorf("empty ssh should be omitted from the YAML:\n%s", b)
	}
}

// `tunnel` is a plain opt-in bool: true round-trips, and absent stays absent
// rather than being written back as `tunnel: false`. A legacy `public: false`
// from the brief tri-state era decodes to the zero value and the key is dropped
// on rewrite — false and absent are the same statement. The fixture is written
// with the PRE-RENAME `public:` key, so this also covers the rename migration.
func TestServiceTunnel_OptIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  wanted:
    fqdn: a.example.com
    host: h
    backend: x:1
    public: true
  legacyfalse:
    fqdn: b.example.com
    host: h
    backend: x:1
    public: false
  undeclared:
    fqdn: c.example.com
    host: h
    backend: x:1
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"wanted": true, "legacyfalse": false, "undeclared": false} {
		if got := c.Services[name].Tunnel; got != want {
			t.Errorf("%s: Tunnel = %v, want %v", name, got, want)
		}
	}

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	// The opt-in round-trips under the CURRENT key: a legacy `public: true`
	// input is re-emitted as `tunnel: true` (one-way rename migration).
	if !strings.Contains(out, "tunnel: true") {
		t.Errorf("opt-in must round-trip as tunnel:\n%s", out)
	}
	// Exactly one `tunnel:` key — the false and the absent both emit nothing.
	if n := strings.Count(out, "tunnel:"); n != 1 {
		t.Errorf("expected exactly 1 tunnel: key, got %d:\n%s", n, out)
	}
	// The pre-rename key is gone after the rewrite.
	if strings.Contains(out, "public:") {
		t.Errorf("legacy public: key must not survive a rewrite:\n%s", out)
	}
}

// Legacy top-level `public_paths` migrates to `auth.bypass_paths` on load and
// the old key is gone after the next rewrite. The paths themselves, and the
// auth mode they belong to, are preserved exactly.
func TestBypassPaths_MigratesFromPublicPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  gatus:
    fqdn: status.example.com
    host: h
    backend: gatus:8080
    auth:
      mode: forward
      groups: [admins]
    public_paths:
      - /health
      - /api/v1/endpoints/*
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Services["gatus"].Auth
	if got.Mode != AuthForward || len(got.Groups) != 1 {
		t.Fatalf("auth mangled by migration: %+v", got)
	}
	if len(got.BypassPaths) != 2 || got.BypassPaths[0] != "/health" || got.BypassPaths[1] != "/api/v1/endpoints/*" {
		t.Fatalf("bypass paths not migrated: %+v", got.BypassPaths)
	}

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	if strings.Contains(out, "public_paths") {
		t.Errorf("legacy public_paths must not be re-emitted:\n%s", out)
	}
	if !strings.Contains(out, "bypass_paths") || !strings.Contains(out, "/health") {
		t.Errorf("bypass_paths should be emitted under auth:\n%s", out)
	}
	// Reloading the rewritten file must yield the same paths (no double migration).
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Services["gatus"].Auth.BypassPaths) != 2 {
		t.Errorf("re-load lost bypass paths: %+v", c2.Services["gatus"].Auth)
	}
}

// An explicit auth.bypass_paths wins over a legacy public_paths if a file
// somehow carries both — the new key is authoritative, never merged.
func TestBypassPaths_ExplicitWinsOverLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  s:
    fqdn: s.example.com
    host: h
    backend: x:1
    auth:
      mode: forward
      bypass_paths: [/new]
    public_paths: [/old]
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Services["s"].Auth.BypassPaths
	if len(got) != 1 || got[0] != "/new" {
		t.Errorf("explicit bypass_paths should win, got %+v", got)
	}
}

// bypass_paths alone (no groups) is enough to force the object form — the terse
// string form cannot carry it.
func TestBypassPaths_ForcesObjectForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  s:
    fqdn: s.example.com
    host: h
    backend: x:1
    auth:
      mode: forward
      bypass_paths: [/health]
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	if !strings.Contains(out, "mode: forward") || !strings.Contains(out, "bypass_paths") {
		t.Errorf("bypass_paths must force the object form:\n%s", out)
	}
	if strings.Contains(out, "auth: forward") {
		t.Errorf("short form cannot carry bypass_paths:\n%s", out)
	}
}

// tunnel_dir resolution: unset means genuinely unset (ok=false) — unlike
// dns_host, there is no auto-fallback to DefaultTunnelDir. A host with a
// public service and no tunnel_dir configured must be refused (planService),
// not silently written to a guessed path.
func TestResolvedTunnelDir(t *testing.T) {
	if dir, ok := (Defaults{}).ResolvedTunnelDir(); ok {
		t.Errorf("unset tunnel_dir should be (\"\", false), got (%q, %v)", dir, ok)
	}
	if dir, ok := (Defaults{TunnelDir: "cloudflared-local/data"}).ResolvedTunnelDir(); !ok || dir != "cloudflared-local/data" {
		t.Errorf("set tunnel_dir should be used verbatim, got (%q, %v)", dir, ok)
	}
}

// external parses, round-trips, and is a distinct non-provider mode.
func TestAuthExternal_ParseAndProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, `hosts: {}
domains: []
defaults: {}
services:
  app:
    fqdn: app.example.com
    host: h
    backend: x:1
    auth: external
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Services["app"].Auth.Mode; got != AuthExternal {
		t.Fatalf("mode = %q, want external", got)
	}
	if AuthExternal.UsesProvider() {
		t.Error("external must NOT use the auth provider")
	}
	if !AuthForward.UsesProvider() || !AuthOIDC.UsesProvider() {
		t.Error("forward and oidc must use the provider")
	}
	if AuthNone.UsesProvider() {
		t.Error("none must not use the provider")
	}
	// Round-trips as the short string form.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "auth: external") {
		t.Errorf("external should round-trip as 'auth: external':\n%s", b)
	}
}

// A per-horizon auth policy round-trips through the object form, and a service
// without one keeps emitting the terse string form.
func TestAuthPolicy_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	content := `hosts:
  box: {ip: 192.0.2.1}
domains:
  - example.com
defaults:
  dns_host: box
  local_networks: [10.0.0.0/8, 192.168.1.0/24]
services:
  media:
    fqdn: media.example.com
    host: box
    backend: jellyfin:8096
    auth:
      mode: forward
      policy: {local: bypass, public: one_factor}
  panel:
    fqdn: panel.example.com
    host: box
    backend: pihole:80
    auth:
      mode: forward
      groups: [admins]
      policy: {public: two_factor}
  plain:
    fqdn: plain.example.com
    host: box
    backend: app:80
    auth: forward
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Defaults.LocalNetworks; len(got) != 2 || got[0] != "10.0.0.0/8" {
		t.Fatalf("local_networks = %v", got)
	}
	media := c.Services["media"].Auth.Policy
	if media.Local != PolicyBypass || media.Public != PolicyOneFactor {
		t.Errorf("media policy = %+v", media)
	}
	// A partial map leaves the other horizon unset, which RESOLVES to the
	// default rather than being an error.
	panel := c.Services["panel"].Auth.Policy
	if panel.Local != "" {
		t.Errorf("panel local should stay unset, got %q", panel.Local)
	}
	if got := panel.ResolveLocal(); got != DefaultPolicy {
		t.Errorf("panel unset local should resolve to the default, got %q", got)
	}
	if panel.ResolvePublic() != PolicyTwoFactor {
		t.Errorf("panel public = %q", panel.ResolvePublic())
	}
	if !c.Services["plain"].Auth.Policy.IsZero() {
		t.Errorf("plain should carry no policy")
	}

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	if !strings.Contains(out, "local: bypass") || !strings.Contains(out, "public: two_factor") {
		t.Errorf("policies must round-trip:\n%s", out)
	}
	// A service with no policy keeps the SHORT auth form — the schema stays
	// terse for the services that don't use this.
	if !strings.Contains(out, "auth: forward") {
		t.Errorf("policy-less service should keep the short auth form:\n%s", out)
	}
}

// An unknown policy value is an error, never a silent default: guessing would
// either widen (bypass) or narrow (deny) what the operator wrote.
func TestAuthPolicy_UnknownLevelRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	content := `hosts:
  box: {ip: 192.0.2.1}
domains:
  - example.com
defaults:
  dns_host: box
services:
  x:
    fqdn: x.example.com
    host: box
    backend: app:80
    auth: {mode: forward, policy: {local: three_factor}}
`
	os.WriteFile(path, []byte(content), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("an unknown policy level must be a parse error")
	} else if !strings.Contains(err.Error(), "three_factor") {
		t.Errorf("error should name the offending value, got: %v", err)
	}
}

// NeedsLocalRule is what decides whether a networks-scoped rule is emitted at
// all: identical horizons need no separate rule, since the catch-all already
// says the same thing.
func TestAuthPolicy_NeedsLocalRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    AuthPolicy
		want bool
	}{
		{"both unset", AuthPolicy{}, false},
		{"explicitly identical", AuthPolicy{Local: PolicyOneFactor, Public: PolicyOneFactor}, false},
		{"local unset, public defaulted", AuthPolicy{Public: PolicyOneFactor}, false},
		{"differing", AuthPolicy{Local: PolicyBypass, Public: PolicyOneFactor}, true},
		{"local only, differs from default", AuthPolicy{Local: PolicyBypass}, true},
		{"public only, differs from default", AuthPolicy{Public: PolicyTwoFactor}, true},
	} {
		if got := tc.p.NeedsLocalRule(); got != tc.want {
			t.Errorf("%s: NeedsLocalRule() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
