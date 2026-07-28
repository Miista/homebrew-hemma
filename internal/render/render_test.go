package render

import (
	"strings"
	"testing"

	"hemma/internal/config"
)

func TestDNSRecord(t *testing.T) {
	got := DNSRecord("docs.example.com", "192.0.2.2")
	want := Header + "\n" +
		"local=/docs.example.com/\n" +
		"address=/docs.example.com/192.0.2.2\n" +
		"address=/docs.example.com/::\n"
	if got != want {
		t.Fatalf("DNSRecord mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// The :: vs ::1 distinction is structural (design §4.1): :: suppresses the
// public AAAA; ::1 is an explicit bug.
func TestDNSRecord_SuppressesAAAAWithUnspecified(t *testing.T) {
	got := DNSRecord("x.example.net", "192.0.2.1")
	if !strings.Contains(got, "address=/x.example.net/::\n") {
		t.Errorf("missing AAAA-suppression line: %q", got)
	}
	if strings.Contains(got, "::1") {
		t.Errorf("emitted ::1 (loopback) — must be :: (unspecified): %q", got)
	}
}

func TestCaddySite(t *testing.T) {
	got := CaddySite("docs.example.com", "tls_example_com", "paperless:8000", config.AuthNone, false)
	want := Header + "\n" +
		"docs.example.com {\n" +
		"\timport tls_example_com\n" +
		"\treverse_proxy paperless:8000\n" +
		"}\n"
	if got != want {
		t.Fatalf("CaddySite mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// oidc renders a PLAIN reverse_proxy with NO `import auth` — the app does OIDC
// itself, so hemma adds no Caddy-level auth gate.
func TestCaddySite_OIDC(t *testing.T) {
	got := CaddySite("app.example.com", "tls_example_com", "app:3000", config.AuthOIDC, false)
	if strings.Contains(got, "import auth") {
		t.Errorf("oidc must NOT import auth: %q", got)
	}
	if !strings.Contains(got, "reverse_proxy app:3000") {
		t.Errorf("oidc should still reverse_proxy: %q", got)
	}
}

func TestCaddySite_Auth(t *testing.T) {
	got := CaddySite("docs.example.com", "tls_example_com", "paperless:8000", config.AuthForward, false)
	want := Header + "\n" +
		"docs.example.com {\n" +
		"\timport tls_example_com\n" +
		"\timport auth\n" +
		"\treverse_proxy paperless:8000\n" +
		"}\n"
	if got != want {
		t.Fatalf("CaddySite(auth) mismatch:\n got: %q\nwant: %q", got, want)
	}
	// The import must precede reverse_proxy so the auth check runs first.
	if strings.Index(got, "import auth") > strings.Index(got, "reverse_proxy") {
		t.Errorf("import auth must come before reverse_proxy: %q", got)
	}
}

// The auth backend (the Authelia portal) preserves the inbound X-Forwarded-Host
// via a header_up inside reverse_proxy, so post-login redirects target the
// original service. It is never itself behind auth (auth=false here).
func TestCaddySite_AuthBackend(t *testing.T) {
	got := CaddySite("auth.example.com", "tls_example_com", "authelia:9091", config.AuthNone, true)
	want := Header + "\n" +
		"auth.example.com {\n" +
		"\timport tls_example_com\n" +
		"\treverse_proxy authelia:9091 {\n" +
		"\t\theader_up X-Forwarded-Host {header.X-Forwarded-Host}\n" +
		"\t}\n" +
		"}\n"
	if got != want {
		t.Fatalf("CaddySite(authBackend) mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// public_paths are NOT rendered in Caddy: the auth provider's generated
// bypass rules are the single policy engine (design §4.5), so a forward
// service renders one uniform gated shape — no handle blocks, no per-path
// branches — regardless of its public_paths.
func TestCaddySite_ForwardIsUniform_NoPublicPathBranches(t *testing.T) {
	got := CaddySite("status.example.com", "tls_example_com", "gatus:8080", config.AuthForward, false)
	want := Header + "\n" +
		"status.example.com {\n" +
		"\timport tls_example_com\n" +
		"\timport auth\n" +
		"\treverse_proxy gatus:8080\n" +
		"}\n"
	if got != want {
		t.Fatalf("CaddySite(forward) mismatch:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "handle") {
		t.Errorf("forward site must contain no handle blocks: %q", got)
	}
	if strings.Count(got, "import auth") != 1 {
		t.Errorf("forward site must import auth exactly once: %q", got)
	}
}

func TestAuthSnippet_EmptyStub(t *testing.T) {
	got := AuthSnippet("")
	want := Header + "\n(auth) {\n}\n"
	if got != want {
		t.Fatalf("empty AuthSnippet mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestAuthSnippet_Body(t *testing.T) {
	body := "forward_auth https://auth.example.com {\n\turi /api/authz/forward-auth\n}"
	got := AuthSnippet(body)
	want := Header + "\n(auth) {\n" +
		"\tforward_auth https://auth.example.com {\n" +
		"\t\turi /api/authz/forward-auth\n" +
		"\t}\n" +
		"}\n"
	if got != want {
		t.Fatalf("AuthSnippet body mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// external renders exactly like none/oidc — a plain reverse_proxy, no import auth.
// It is a declaration, not a gate.
//
// All THREE non-forward modes must be byte-identical: forward is the only one
// where hemma itself is the gate. They still differ in services.yaml and in
// `list`, which is the point — identical Caddy output must not collapse into an
// identical-LOOKING service.
func TestCaddySite_ExternalRendersPlain(t *testing.T) {
	site := func(m config.AuthMode) string {
		return CaddySite("app.example.com", "tls_example_com", "app:3000", m, false)
	}
	none, oidc, ext := site(config.AuthNone), site(config.AuthOIDC), site(config.AuthExternal)
	if ext != none {
		t.Errorf("external must render identically to none:\n--- external ---\n%s\n--- none ---\n%s", ext, none)
	}
	if ext != oidc {
		t.Errorf("external must render identically to oidc:\n--- external ---\n%s\n--- oidc ---\n%s", ext, oidc)
	}
	for m, out := range map[config.AuthMode]string{
		config.AuthNone: none, config.AuthOIDC: oidc, config.AuthExternal: ext,
	} {
		if strings.Contains(out, "import "+AuthSnippetName) {
			t.Errorf("mode %q must not emit an auth gate:\n%s", m, out)
		}
	}
	// forward must NOT match them — without this, the assertions above would
	// still pass on a build where the gate had been dropped entirely.
	if fwd := site(config.AuthForward); fwd == none {
		t.Errorf("forward must differ from the ungated modes:\n%s", fwd)
	}
}

func TestCloudflaredConfig(t *testing.T) {
	got := CloudflaredConfig([]CloudflaredIngressEntry{
		{Hostname: "auth.example.com", Backend: "https://caddy:443"},
		{Hostname: "docs.example.com", Backend: "https://caddy:443"},
	})
	want := Header + "\n" +
		"ingress:\n" +
		"  - hostname: auth.example.com\n" +
		"    service: https://caddy:443\n" +
		"  - hostname: docs.example.com\n" +
		"    service: https://caddy:443\n" +
		"  - service: http_status:404\n"
	if got != want {
		t.Fatalf("CloudflaredConfig mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// Even with no entries, the mandatory catch-all is always present and last —
// cloudflared requires ingress rules to end in one, and match order matters.
func TestCloudflaredConfig_Empty(t *testing.T) {
	got := CloudflaredConfig(nil)
	want := Header + "\ningress:\n  - service: http_status:404\n"
	if got != want {
		t.Fatalf("CloudflaredConfig(nil) = %q, want %q", got, want)
	}
}
