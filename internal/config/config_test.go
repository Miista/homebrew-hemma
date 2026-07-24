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
	c.Services["docs"] = Service{FQDN: "docs.example.com", Host: "resolver", Backend: "paperless:8000"}
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
	if c2.Services["docs"].Backend != "paperless:8000" {
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
	write(t, path, "hosts:\n  pi: {ip: 192.0.2.1}\ndomains: []\ndefaults: {}\nservices: {}\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Default: no ssh field → destination is the host name.
	if got := c.Hosts["pi"].SSHDest("pi"); got != "pi" {
		t.Errorf("SSHDest default should be the host name, got %q", got)
	}
	// Set a verbatim destination and round-trip it.
	c.Hosts["pi"] = Host{IP: "192.0.2.1", SSH: "admin@raspberry.local"}
	c.Hosts["box"] = Host{IP: "192.0.2.2"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Hosts["pi"].SSH; got != "admin@raspberry.local" {
		t.Errorf("ssh field should round-trip, got %q", got)
	}
	if got := c2.Hosts["pi"].SSHDest("pi"); got != "admin@raspberry.local" {
		t.Errorf("SSHDest should return the explicit destination, got %q", got)
	}
	// A host without ssh must not gain the key on disk (omitempty).
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "box: {ip: 192.0.2.2, ssh") || strings.Count(string(b), "ssh:") != 1 {
		t.Errorf("empty ssh should be omitted from the YAML:\n%s", b)
	}
}

// The `public` field is three-state: a declared true/false round-trips, and an
// absent key stays absent rather than defaulting to false. The distinction is
// load-bearing — doctor only compares DECLARED intent against the observed
// compose label, so an undeclared service must not read as "declared internal".
func TestServicePublic_ThreeState(t *testing.T) {
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
  internal:
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
	for name, want := range map[string]struct{ val, declared bool }{
		"wanted":     {true, true},
		"internal":   {false, true},
		"undeclared": {false, false},
	} {
		got, declared := c.Services[name].DeclaredPublic()
		if got != want.val || declared != want.declared {
			t.Errorf("%s: DeclaredPublic() = (%v, %v), want (%v, %v)", name, got, declared, want.val, want.declared)
		}
	}

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	if !strings.Contains(out, "public: true") {
		t.Errorf("declared true must round-trip:\n%s", out)
	}
	if !strings.Contains(out, "public: false") {
		t.Errorf("declared false must round-trip (it is not the zero value):\n%s", out)
	}
	// The undeclared service must not gain a `public:` key on rewrite.
	if n := strings.Count(out, "public:"); n != 2 {
		t.Errorf("expected exactly 2 public: keys (undeclared stays absent), got %d:\n%s", n, out)
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

// public_label resolution: unset falls back to the cloudflared default, "none"
// disables the lookup, anything else is used verbatim.
func TestResolvedPublicLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":                  DefaultPublicLabel,
		PublicLabelDisabled: "",
		"my.tunnel/host":    "my.tunnel/host",
	} {
		if got := (Defaults{PublicLabel: in}).ResolvedPublicLabel(); got != want {
			t.Errorf("ResolvedPublicLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
