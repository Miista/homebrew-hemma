package plan

import (
	"strings"
	"testing"

	"hemma/internal/config"
)

// forward_gate (§4.9) is only for an oidc app on the tunnel; anywhere else the entry is skipped with a reason, before
// anything is generated for it.
func TestBuild_ForwardGateOnlyForOIDCOnTheTunnel(t *testing.T) {
	for _, c := range []struct {
		name string
		svc  config.Service
	}{
		{"oidc off the tunnel", config.Service{Auth: config.Auth{Mode: config.AuthOIDC, ForwardGate: true}}},
		{"forward", config.Service{Tunnel: true, Auth: config.Auth{Mode: config.AuthForward, ForwardGate: true}}},
		{"external", config.Service{Tunnel: true, Auth: config.Auth{Mode: config.AuthExternal, ForwardGate: true}}},
		{"none", config.Service{Tunnel: true, Auth: config.Auth{Mode: config.AuthNone, ForwardGate: true}}},
	} {
		cfg := base()
		svc := c.svc
		svc.FQDN, svc.Host, svc.Backend = "x.example.com", "appbox", "app:80"
		cfg.Services["x"] = svc
		reason, skipped := Build(cfg).Skipped["x"]
		if !skipped || !strings.Contains(reason, "forward_gate") {
			t.Errorf("%s: want skipped for forward_gate, got skipped=%v %q", c.name, skipped, reason)
		}
	}
}

// A gated oidc app: Caddy asks the provider only for requests from outside local_networks, and the provider gets the
// forward rules for it (the local horizon fixed at bypass, the catch-all with the public policy and the groups, the bypass
// paths) next to the OIDC policy that still governs the app's own login.
func TestBuild_ForwardGate(t *testing.T) {
	c := base()
	c.Defaults.AuthService = "portal"
	c.Defaults.LocalNetworks = []string{"10.0.0.0/8"}
	c.Services["portal"] = config.Service{FQDN: "auth.example.com", Host: "appbox", Backend: "authelia:9091"}
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000", Tunnel: true,
		Auth: config.Auth{Mode: config.AuthOIDC, ForwardGate: true, Groups: []string{"family"}, BypassPaths: []string{"/api"},
			Policy: config.AuthPolicy{Public: config.PolicyTwoFactor}}}
	p := Build(c)
	if reason, skipped := p.Skipped["docs"]; skipped {
		t.Fatalf("a gated oidc app should plan, skipped: %s", reason)
	}
	var site string
	for _, f := range p.Files["docs"] {
		if strings.HasSuffix(f.Path, ".caddy") {
			site = f.Content
		}
	}
	for _, want := range []string{"@hemma_outside not client_ip 10.0.0.0/8", "route @hemma_outside {\n\t\timport auth\n\t}"} {
		if !strings.Contains(site, want) {
			t.Errorf("site missing %q:\n%s", want, site)
		}
	}
	ac := p.Files[authAccessKey][0].Content
	gate := "    - domain: 'docs.example.com'\n      resources:\n        - '^/api(\\?.*)?$'\n      policy: 'bypass'\n" +
		"    - domain: 'docs.example.com'\n      networks:\n        - '10.0.0.0/8'\n      policy: 'bypass'\n" +
		"    - domain: 'docs.example.com'\n      policy: 'two_factor'\n      subject:\n        - 'group:family'\n"
	if !strings.Contains(ac, "access_control:\n") || !strings.Contains(ac, gate) {
		t.Errorf("access_control must carry the gate's rules:\n%s", ac)
	}
	if !strings.Contains(ac, "authorization_policies:\n      docs:") {
		t.Errorf("the OIDC policy must stay for the app's own login:\n%s", ac)
	}

	// the same app without the gate: a plain proxy, and no access_control rules for it
	svc := c.Services["docs"]
	svc.Auth.ForwardGate = false
	c.Services["docs"] = svc
	p = Build(c)
	for _, f := range p.Files["docs"] {
		if strings.Contains(f.Content, "import auth") {
			t.Errorf("without the gate an oidc site is a plain proxy:\n%s", f.Content)
		}
	}
	if strings.Contains(p.Files[authAccessKey][0].Content, "access_control:") {
		t.Errorf("without the gate there are no access_control rules for an oidc app:\n%s", p.Files[authAccessKey][0].Content)
	}
}

// forward with a local bypass renders the same scoped site (Caddy no longer asks the provider for a local request), while
// its access-control rules stay as they were.
func TestBuild_ForwardLocalBypassSkipsTheProviderLocally(t *testing.T) {
	c := base()
	c.Defaults.LocalNetworks = []string{"10.0.0.0/8"}
	c.Services["media"] = config.Service{FQDN: "media.example.com", Host: "appbox", Backend: "jellyfin:8096", Tunnel: true,
		Auth: config.Auth{Mode: config.AuthForward, Policy: config.AuthPolicy{Local: config.PolicyBypass, Public: config.PolicyOneFactor}}}
	for _, f := range Build(c).Files["media"] {
		if strings.HasSuffix(f.Path, ".caddy") && !strings.Contains(f.Content, "route @hemma_outside {") {
			t.Errorf("forward + local bypass should ask the provider only from outside:\n%s", f.Content)
		}
	}
}
