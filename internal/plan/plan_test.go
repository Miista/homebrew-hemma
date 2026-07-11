package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// base returns a valid config with one service we can mutate per test.
func base() *config.Config {
	return &config.Config{
		Hosts: map[string]config.Host{
			"resolver": {IP: "192.0.2.1", Dir: "resolver"},
			"appbox":   {IP: "192.0.2.2", Dir: "appbox"},
		},
		Domains: map[string]config.Domain{
			"example.com": {},
			"example.net": {},
		},
		Defaults: config.Defaults{DNSHost: "resolver"},
		Services: map[string]config.Service{},
	}
}

func TestBuild_ValidService(t *testing.T) {
	c := base()
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000"}

	p := Build(c)
	if len(p.Skipped) != 0 {
		t.Fatalf("unexpected skips: %v", p.Skipped)
	}
	files := p.Files["docs"]
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}

	byExt := map[string]File{}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, ".conf"):
			byExt["conf"] = f
		case strings.HasSuffix(f.Path, ".caddy"):
			byExt["caddy"] = f
		}
	}

	// DNS goes to the dns_host (resolver) dir; A record points at the host (appbox) IP.
	conf := byExt["conf"]
	if conf.Path != "resolver/"+config.DefaultDnsmasqDir+"/docs.generated.conf" {
		t.Errorf("dns path wrong: %q", conf.Path)
	}
	if !strings.Contains(conf.Content, "192.0.2.2") {
		t.Errorf("A record should point at host (appbox) IP: %q", conf.Content)
	}

	// Caddy goes to the host (appbox) dir with the matched tls import.
	caddy := byExt["caddy"]
	if caddy.Path != "appbox/"+config.DefaultCaddySitesDir+"/docs.caddy" {
		t.Errorf("caddy path wrong: %q", caddy.Path)
	}
	if !strings.Contains(caddy.Content, "import tls_example_com") {
		t.Errorf("wrong tls import: %q", caddy.Content)
	}
}

// The (auth) snippet file is always planned on every host, regardless of
// whether any service opts in. Empty body → empty stub.
func TestBuild_AuthSnippetAlwaysPresent(t *testing.T) {
	c := base()
	p := Build(c)
	files := p.Files[authSnippetKey]
	if len(files) != len(c.Hosts) {
		t.Fatalf("expected one auth snippet per host (%d), got %d", len(c.Hosts), len(files))
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "caddy/data/hemma.auth.generated.caddy") {
			t.Errorf("auth snippet path wrong: %q", f.Path)
		}
		if !strings.Contains(f.Content, "(auth) {\n}") {
			t.Errorf("expected empty (auth) stub, got: %q", f.Content)
		}
	}
	if !IsSyntheticOwner(authSnippetKey) {
		t.Errorf("%q should be a synthetic owner", authSnippetKey)
	}
}

// A service with Auth:true emits `import auth`; the snippet body flows into the
// generated (auth) file.
func TestBuild_ServiceAuthImportsSnippet(t *testing.T) {
	c := base()
	c.AuthSnippetBody = "forward_auth https://auth.example.com {\n\turi /api/authz/forward-auth\n}"
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000", Auth: config.Auth{Mode: config.AuthForward}}

	p := Build(c)
	if len(p.Skipped) != 0 {
		t.Fatalf("unexpected skips: %v", p.Skipped)
	}
	var caddy File
	for _, f := range p.Files["docs"] {
		if strings.HasSuffix(f.Path, ".caddy") {
			caddy = f
		}
	}
	if !strings.Contains(caddy.Content, "\timport auth\n") {
		t.Errorf("auth service should import auth: %q", caddy.Content)
	}
	// Body copied into the (auth) file.
	if !strings.Contains(p.Files[authSnippetKey][0].Content, "forward_auth https://auth.example.com") {
		t.Errorf("auth body not in snippet: %q", p.Files[authSnippetKey][0].Content)
	}
}

// An oidc service renders a PLAIN reverse_proxy (no `import auth`) — the app
// authenticates itself, so hemma adds no Caddy-level gate.
func TestBuild_ServiceOIDCPlainProxy(t *testing.T) {
	c := base()
	c.Services["app"] = config.Service{FQDN: "app.example.com", Host: "appbox", Backend: "app:3000", Auth: config.Auth{Mode: config.AuthOIDC}}

	p := Build(c)
	if _, skipped := p.Skipped["app"]; skipped {
		t.Fatalf("oidc service should not be skipped: %v", p.Skipped)
	}
	var caddy File
	for _, f := range p.Files["app"] {
		if strings.HasSuffix(f.Path, ".caddy") {
			caddy = f
		}
	}
	if strings.Contains(caddy.Content, "import auth") {
		t.Errorf("oidc service must NOT import auth: %q", caddy.Content)
	}
	if !strings.Contains(caddy.Content, "reverse_proxy app:3000") {
		t.Errorf("oidc service should reverse_proxy: %q", caddy.Content)
	}
}

// Loop guard: the service named as auth_service (the auth backend) must be
// skipped if it also sets any auth mode (forward OR oidc) — protecting the
// portal loops.
func TestBuild_AuthLoopGuardOIDC(t *testing.T) {
	c := base()
	c.Defaults.AuthService = "portal"
	c.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091", Auth: config.Auth{Mode: config.AuthOIDC}}
	if _, skipped := Build(c).Skipped["portal"]; !skipped {
		t.Fatalf("oidc auth_service should be skipped by the loop guard")
	}
}

// Loop guard: the service named as auth_service (the auth backend) must be
// skipped if it also sets auth:true — protecting the portal loops.
func TestBuild_AuthLoopGuard(t *testing.T) {
	c := base()
	c.Defaults.AuthService = "portal"
	c.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091", Auth: config.Auth{Mode: config.AuthForward}}

	p := Build(c)
	reason, skipped := p.Skipped["portal"]
	if !skipped {
		t.Fatalf("expected portal to be skipped by loop guard")
	}
	if !strings.Contains(reason, "redirect loop") {
		t.Errorf("skip reason should mention the loop: %q", reason)
	}
	// Same service WITHOUT auth is fine (it's the backend, not protected).
	c2 := base()
	c2.Defaults.AuthService = "portal"
	c2.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091"}
	if _, skipped := Build(c2).Skipped["portal"]; skipped {
		t.Errorf("portal without auth should not be skipped")
	}
}

// The auth_service's site block preserves X-Forwarded-Host; other blocks don't.
func TestBuild_AuthServiceHeaderPreserve(t *testing.T) {
	c := base()
	c.Defaults.AuthService = "portal"
	c.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091"}
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000"}

	p := Build(c)
	caddyOf := func(svc string) string {
		for _, f := range p.Files[svc] {
			if strings.HasSuffix(f.Path, ".caddy") {
				return f.Content
			}
		}
		return ""
	}
	if !strings.Contains(caddyOf("portal"), "header_up X-Forwarded-Host {header.X-Forwarded-Host}") {
		t.Errorf("auth_service block should preserve X-Forwarded-Host:\n%s", caddyOf("portal"))
	}
	if strings.Contains(caddyOf("docs"), "X-Forwarded-Host") {
		t.Errorf("non-auth-backend block should NOT preserve X-Forwarded-Host:\n%s", caddyOf("docs"))
	}
}

func TestBuild_SkipReasons(t *testing.T) {
	tests := []struct {
		name    string
		svc     config.Service
		wantSub string
	}{
		{"malformed-fqdn", config.Service{FQDN: "", Host: "resolver", Backend: "a:1"}, "malformed fqdn"},
		{"no-domain-match", config.Service{FQDN: "x.example.org", Host: "resolver", Backend: "a:1"}, "matches no domain"},
		{"unknown-host", config.Service{FQDN: "x.example.com", Host: "nope", Backend: "a:1"}, "unknown host"},
		{"bad-backend", config.Service{FQDN: "x.example.com", Host: "resolver", Backend: "noport"}, "name:port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			c.Services["svc"] = tt.svc
			p := Build(c)
			reason, ok := p.Skipped["svc"]
			if !ok {
				t.Fatalf("expected skip, got files: %+v", p.Files["svc"])
			}
			if !strings.Contains(reason, tt.wantSub) {
				t.Errorf("reason %q does not contain %q", reason, tt.wantSub)
			}
		})
	}
}

func TestBuild_FQDNCollision_FailsBoth(t *testing.T) {
	c := base()
	c.Services["a"] = config.Service{FQDN: "dup.example.com", Host: "resolver", Backend: "x:1"}
	c.Services["b"] = config.Service{FQDN: "dup.example.com", Host: "appbox", Backend: "y:2"}

	p := Build(c)
	if _, ok := p.Skipped["a"]; !ok {
		t.Error("service a should be skipped on fqdn collision")
	}
	if _, ok := p.Skipped["b"]; !ok {
		t.Error("service b should be skipped on fqdn collision")
	}
	for k := range p.Files {
		if !IsSyntheticOwner(k) {
			t.Errorf("no service files should be produced on collision, got %q: %+v", k, p.Files[k])
		}
	}
}

func TestBuild_LongestDomainSuffixWins(t *testing.T) {
	c := base()
	c.Domains["sub.example.com"] = config.Domain{}
	c.Services["s"] = config.Service{FQDN: "a.sub.example.com", Host: "resolver", Backend: "x:1"}

	p := Build(c)
	files := p.Files["s"]
	if len(files) == 0 {
		t.Fatalf("expected files, skipped: %v", p.Skipped)
	}
	var caddy string
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".caddy") {
			caddy = f.Content
		}
	}
	if !strings.Contains(caddy, "import tls_sub") {
		t.Errorf("longest suffix (sub.example.com) should win: %q", caddy)
	}
}

// PinAuthSnippetToDisk rewrites planned auth-snippet content to whatever is on
// disk, so a keep-last-good sync writes it back unchanged rather than resetting
// to the stub. Files not on disk keep their planned (stub) content.
func TestPinAuthSnippetToDisk(t *testing.T) {
	repo := t.TempDir()
	c := base()
	p := Build(c) // empty stub content for each host

	// Write a "last-good" file to disk for the first host's auth snippet path.
	target := p.Files[authSnippetKey][0]
	abs := filepath.Join(repo, target.Path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	lastGood := "# GENERATED by hemma — do not edit. Source: services.yaml\n(auth) {\n\tforward_auth https://auth.example.com { }\n}\n"
	if err := os.WriteFile(abs, []byte(lastGood), 0o644); err != nil {
		t.Fatal(err)
	}

	PinAuthSnippetToDisk(p, repo)

	if p.Files[authSnippetKey][0].Content != lastGood {
		t.Errorf("pinned content should match disk:\n got: %q\nwant: %q",
			p.Files[authSnippetKey][0].Content, lastGood)
	}
	// A path with no file on disk keeps the planned stub (base() has 2 hosts).
	if !strings.Contains(p.Files[authSnippetKey][1].Content, "(auth) {\n}") {
		t.Errorf("absent-on-disk path should keep stub, got %q", p.Files[authSnippetKey][1].Content)
	}
}

// Groups grant access via the auth provider's generated rules; with mode none
// there is no gate for them to apply to — report-but-proceed skip (design §7).
func TestBuild_AuthGroupsOnNoneSkipped(t *testing.T) {
	c := base()
	c.Services["svc"] = config.Service{FQDN: "x.example.com", Host: "appbox", Backend: "a:1", Auth: config.Auth{Groups: []string{"admins"}}}
	p := Build(c)
	reason, ok := p.Skipped["svc"]
	if !ok {
		t.Fatalf("expected skip, got files: %+v", p.Files["svc"])
	}
	if !strings.Contains(reason, "auth groups set but auth mode is none") {
		t.Errorf("wrong skip reason: %q", reason)
	}
}

// The auth provider's access-control artifact (design §4.6) is planned on the
// auth_service's host under the @auth-access synthetic owner — only when an
// auth_service is set and some service actually uses forward auth (or oidc
// with groups).
func TestBuild_AccessControlArtifact(t *testing.T) {
	c := base()
	c.Defaults.AuthService = "portal"
	c.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091"}
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000", Auth: config.Auth{Mode: config.AuthForward, Groups: []string{"admins"}}}

	p := Build(c)
	files := p.Files[authAccessKey]
	if len(files) != 1 {
		t.Fatalf("expected 1 access-control file, got %+v", files)
	}
	wantPath := "appbox/authelia/data/config/hemma.access_control.generated.yml"
	if files[0].Path != wantPath {
		t.Errorf("path %q, want %q", files[0].Path, wantPath)
	}
	if !strings.Contains(files[0].Content, "domain: 'docs.example.com'") ||
		!strings.Contains(files[0].Content, "'group:admins'") {
		t.Errorf("unexpected content:\n%s", files[0].Content)
	}
	if !IsSyntheticOwner(authAccessKey) {
		t.Errorf("%q should be a synthetic owner", authAccessKey)
	}
}

// No auth_service, or no forward/oidc-with-groups service → no artifact (a
// previously generated one becomes an orphan and is GC'd).
func TestBuild_AccessControlArtifactOmitted(t *testing.T) {
	// No auth_service set.
	c := base()
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000", Auth: config.Auth{Mode: config.AuthForward}}
	if files := Build(c).Files[authAccessKey]; files != nil {
		t.Errorf("no auth_service → no artifact, got %+v", files)
	}
	// auth_service set but nothing to cover (oidc without groups).
	c2 := base()
	c2.Defaults.AuthService = "portal"
	c2.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091"}
	c2.Services["app"] = config.Service{FQDN: "app.example.com", Host: "appbox", Backend: "app:3000", Auth: config.Auth{Mode: config.AuthOIDC}}
	if files := Build(c2).Files[authAccessKey]; files != nil {
		t.Errorf("no forward/oidc-with-groups service → no artifact, got %+v", files)
	}
}
