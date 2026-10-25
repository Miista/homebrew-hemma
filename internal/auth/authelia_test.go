package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hemma/internal/render"
)

func provider(t *testing.T) Provider {
	t.Helper()
	p, ok := Lookup("authelia")
	if !ok {
		t.Fatal("authelia provider not registered")
	}
	return p
}

// Golden-style: one forward service with public paths + a single group, in
// stable order — bypass rules first, then the access rule.
func TestAuthelia_AccessControl(t *testing.T) {
	p := provider(t)
	path, got, ok := p.AccessControl([]Service{
		{Name: "status", FQDN: "status.example.com", Mode: ModeForward, BypassPaths: []string{"/health"}},
		{Name: "pihole", FQDN: "pihole.example.com", Mode: ModeForward, Groups: []string{"admins"}},
	})
	if !ok {
		t.Fatal("expected an artifact")
	}
	if path != filepath.Join("authelia/data/config", "hemma.access_control.generated.yml") {
		t.Errorf("path wrong: %q", path)
	}
	want := render.Header + "\n" +
		"access_control:\n" +
		"  default_policy: 'deny'\n" +
		"  rules:\n" +
		"    - domain: 'pihole.example.com'\n" +
		"      policy: 'one_factor'\n" +
		"      subject:\n" +
		"        - 'group:admins'\n" +
		"    - domain: 'status.example.com'\n" +
		"      resources:\n" +
		"        - '^/health(\\?.*)?$'\n" +
		"      policy: 'bypass'\n" +
		"    - domain: 'status.example.com'\n" +
		"      policy: 'one_factor'\n"
	if got != want {
		t.Fatalf("access_control mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// Authelia subject semantics: a flat list of strings is an AND of criteria;
// OR requires nested lists. Multiple groups must be OR'd (membership in ANY
// group grants access) → nested single-element lists.
func TestAuthelia_MultipleGroupsAreORed(t *testing.T) {
	p := provider(t)
	_, got, _ := p.AccessControl([]Service{
		{Name: "docs", FQDN: "docs.example.com", Mode: ModeForward, Groups: []string{"admins", "family"}},
	})
	want := "      subject:\n" +
		"        - ['group:admins']\n" +
		"        - ['group:family']\n"
	if !strings.Contains(got, want) {
		t.Errorf("multiple groups must be nested lists (OR):\n%s", got)
	}
	if strings.Contains(got, "- 'group:admins'\n        - 'group:family'") {
		t.Errorf("flat list of groups would be AND'd by Authelia:\n%s", got)
	}
}

// These regexes ARE the auth exemption (Caddy renders no public_paths gate),
// so the two shapes matter: without a trailing /* the entry is the exact path
// only (query string still allowed — Authelia matches path+query); with /*
// it is the path itself and everything below. Regex meta in the literal path
// is escaped.
func TestAuthelia_PathResourceTranslation(t *testing.T) {
	cases := map[string]string{
		"/health":    `^/health(\?.*)?$`,
		"/api/*":     "^/api([/?].*)?$",
		"/f.o (x)":   `^/f\.o \(x\)(\?.*)?$`,
		"/metrics/*": "^/metrics([/?].*)?$",
	}
	for in, want := range cases {
		if got := pathResource(in); got != want {
			t.Errorf("pathResource(%q) = %q, want %q", in, got, want)
		}
	}
}

// oidc services with groups get a named authorization_policy; oidc without
// groups contributes nothing; the section is omitted entirely when empty.
func TestAuthelia_OIDCAuthorizationPolicies(t *testing.T) {
	p := provider(t)
	_, got, ok := p.AccessControl([]Service{
		{Name: "app", FQDN: "app.example.com", Mode: ModeOIDC, Groups: []string{"users"}},
	})
	if !ok {
		t.Fatal("oidc-with-groups should emit an artifact")
	}
	want := "identity_providers:\n" +
		"  oidc:\n" +
		"    authorization_policies:\n" +
		"      app:\n" +
		"        default_policy: 'deny'\n" +
		"        rules:\n" +
		"          - policy: 'one_factor'\n" +
		"            subject:\n" +
		"              - 'group:users'\n"
	if !strings.Contains(got, want) {
		t.Errorf("authorization_policies mismatch:\n%s", got)
	}
	if strings.Contains(got, "access_control:") {
		t.Errorf("no forward services → no access_control section:\n%s", got)
	}

	// forward-only input → no identity_providers section.
	_, fwdOnly, _ := p.AccessControl([]Service{{Name: "d", FQDN: "d.example.com", Mode: ModeForward}})
	if strings.Contains(fwdOnly, "identity_providers") {
		t.Errorf("no oidc-with-groups → no identity_providers section:\n%s", fwdOnly)
	}

	// oidc without groups or policy → nothing at all.
	if _, _, ok := p.AccessControl([]Service{{Name: "a", FQDN: "a.example.com", Mode: ModeOIDC}}); ok {
		t.Error("oidc without groups should emit no artifact")
	}
}

// A per-horizon policy on oidc: the local rule first (first match wins), then
// the catch-all. With groups the catch-all is scoped by subject; without, by
// every address (a rule with neither is invalid in Authelia).
func TestAuthelia_OIDCPerHorizonPolicy(t *testing.T) {
	p := provider(t)
	_, got, _ := p.AccessControl([]Service{{Name: "app", FQDN: "app.example.com", Mode: ModeOIDC,
		Groups: []string{"admins"}, LocalNetworks: []string{"10.0.0.0/8"}, LocalPolicy: "one_factor", PublicPolicy: "two_factor"}})
	want := "      app:\n" +
		"        default_policy: 'deny'\n" +
		"        rules:\n" +
		"          - policy: 'one_factor'\n" +
		"            networks:\n" +
		"              - '10.0.0.0/8'\n" +
		"            subject:\n" +
		"              - 'group:admins'\n" +
		"          - policy: 'two_factor'\n" +
		"            subject:\n" +
		"              - 'group:admins'\n"
	if !strings.Contains(got, want) {
		t.Errorf("with groups:\n%s", got)
	}

	_, got, ok := p.AccessControl([]Service{{Name: "app", FQDN: "app.example.com", Mode: ModeOIDC, PublicPolicy: "two_factor"}})
	if !ok {
		t.Fatal("a non-default public policy alone should emit a policy")
	}
	want = "          - policy: 'two_factor'\n" +
		"            networks:\n" +
		"              - '0.0.0.0/0'\n" +
		"              - '::/0'\n"
	if !strings.Contains(got, want) {
		t.Errorf("without groups:\n%s", got)
	}
}

// ValidateConfig: an oidc service with groups whose matching client does not
// reference the generated authorization_policy warns; setting it clears the
// warning. Client existence checks are unchanged.
func TestAuthelia_ValidateConfig_AuthorizationPolicy(t *testing.T) {
	p := provider(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configuration.yml")
	svcs := []Service{{Name: "app", FQDN: "app.example.com", Mode: ModeOIDC, Groups: []string{"users"}}}

	write := func(policyLine string) {
		body := "identity_providers:\n  oidc:\n    clients:\n      - client_id: app\n" + policyLine +
			"        redirect_uris:\n          - https://app.example.com/login\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Client exists but no authorization_policy → warn.
	write("")
	w := p.ValidateConfig(cfgPath, svcs)
	if len(w) != 1 || !strings.Contains(w[0].String(), "authorization_policy: 'app'") {
		t.Errorf("expected authorization_policy advisory carrying the paste-in line, got %v", w)
	}

	// Wrong policy name → still warns.
	write("        authorization_policy: other\n")
	if w := p.ValidateConfig(cfgPath, svcs); len(w) != 1 {
		t.Errorf("wrong policy name should warn, got %v", w)
	}

	// Correct policy → clean.
	write("        authorization_policy: app\n")
	if w := p.ValidateConfig(cfgPath, svcs); len(w) != 0 {
		t.Errorf("expected no warnings, got %v", w)
	}

	// No groups → no policy expectation, even without authorization_policy.
	write("")
	if w := p.ValidateConfig(cfgPath, []Service{{Name: "app", FQDN: "app.example.com", Mode: ModeOIDC}}); len(w) != 0 {
		t.Errorf("groupless oidc should not demand a policy, got %v", w)
	}

	// Missing client is still the existing warning.
	if err := os.WriteFile(cfgPath, []byte("identity_providers:\n  oidc:\n    clients: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w = p.ValidateConfig(cfgPath, svcs)
	if len(w) != 1 || !strings.Contains(w[0].String(), "no Authelia OIDC client registers a redirect_uri") {
		t.Errorf("expected missing-client advisory, got %v", w)
	}
}

// Multiple services must nest under ONE access_control section and ONE
// identity_providers.oidc.authorization_policies section — YAML forbids
// repeated top-level keys, so per-service sections would be a broken file.
func TestAuthelia_SingleSectionPerKind(t *testing.T) {
	p := provider(t)
	_, got, ok := p.AccessControl([]Service{
		{Name: "b-app", FQDN: "b.example.com", Mode: ModeOIDC, Groups: []string{"users"}},
		{Name: "a-app", FQDN: "a.example.com", Mode: ModeOIDC, Groups: []string{"admins"}},
		{Name: "docs", FQDN: "docs.example.com", Mode: ModeForward},
		{Name: "pihole", FQDN: "pihole.example.com", Mode: ModeForward, Groups: []string{"admins"}},
	})
	if !ok {
		t.Fatal("expected an artifact")
	}
	for _, key := range []string{"access_control:", "identity_providers:", "  oidc:", "    authorization_policies:"} {
		if n := strings.Count(got, key+"\n"); n != 1 {
			t.Errorf("%q must appear exactly once, got %d:\n%s", key, n, got)
		}
	}
	// Both policies nested under the single section, alphabetical.
	if !strings.Contains(got, "    authorization_policies:\n      a-app:\n") {
		t.Errorf("a-app policy missing/misplaced:\n%s", got)
	}
	ia, ib := strings.Index(got, "      a-app:"), strings.Index(got, "      b-app:")
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("policies must both nest under the one section, a before b:\n%s", got)
	}
	// Both forward rules inside the single rules list, alphabetical.
	id, ip := strings.Index(got, "- domain: 'docs.example.com'"), strings.Index(got, "- domain: 'pihole.example.com'")
	if id < 0 || ip < 0 || id > ip {
		t.Errorf("forward rules must both be in the one rules list, docs before pihole:\n%s", got)
	}
}

func TestAutheliaApplyCommands(t *testing.T) {
	validate, reload := authelia{}.ApplyCommands("authelia")
	wantV := []string{"docker", "exec", "authelia", "authelia", "config", "validate"}
	wantR := []string{"docker", "restart", "authelia"}
	if !reflect.DeepEqual(validate, wantV) {
		t.Errorf("validate = %v, want %v", validate, wantV)
	}
	if !reflect.DeepEqual(reload, wantR) {
		t.Errorf("reload = %v, want %v", reload, wantR)
	}
}

// A differing local policy emits a networks-scoped rule BEFORE the catch-all
// (Authelia is first-match), and the catch-all carries the public policy.
func TestAccessControl_PerHorizonPolicy(t *testing.T) {
	_, content, ok := authelia{}.AccessControl([]Service{{
		Name: "media", FQDN: "media.example.com", Mode: ModeForward,
		LocalNetworks: []string{"10.0.0.0/8", "192.168.1.0/24"},
		LocalPolicy:   "bypass",
		PublicPolicy:  "one_factor",
	}})
	if !ok {
		t.Fatal("expected content")
	}
	want := `access_control:
  default_policy: 'deny'
  rules:
    - domain: 'media.example.com'
      networks:
        - '10.0.0.0/8'
        - '192.168.1.0/24'
      policy: 'bypass'
    - domain: 'media.example.com'
      policy: 'one_factor'
`
	if !strings.Contains(content, want) {
		t.Errorf("rules mismatch:\ngot:\n%s\nwant to contain:\n%s", content, want)
	}
}

// A bypass rule must carry NO subject: Authelia documents that subject "may
// not be used for the bypass policy" (identifying a subject needs at least
// one_factor), so emitting both would be invalid config. Groups therefore
// apply only to the horizon that actually authenticates.
func TestAccessControl_BypassRuleOmitsSubject(t *testing.T) {
	_, content, _ := authelia{}.AccessControl([]Service{{
		Name: "media", FQDN: "media.example.com", Mode: ModeForward,
		Groups:        []string{"media-users"},
		LocalNetworks: []string{"10.0.0.0/8"},
		LocalPolicy:   "bypass",
		PublicPolicy:  "one_factor",
	}})
	local := content[strings.Index(content, "networks:"):strings.LastIndex(content, "- domain:")]
	if strings.Contains(local, "subject:") {
		t.Errorf("bypass rule must not carry a subject:\n%s", content)
	}
	// ...but the authenticating catch-all still restricts by group.
	tail := content[strings.LastIndex(content, "- domain:"):]
	if !strings.Contains(tail, "group:media-users") {
		t.Errorf("public rule should still carry the subject:\n%s", content)
	}
}

// A local policy that authenticates DOES carry the subject — networks and
// subject combine fine when the policy is not bypass.
func TestAccessControl_AuthenticatingLocalRuleKeepsSubject(t *testing.T) {
	_, content, _ := authelia{}.AccessControl([]Service{{
		Name: "panel", FQDN: "panel.example.com", Mode: ModeForward,
		Groups:        []string{"admins"},
		LocalNetworks: []string{"10.0.0.0/8"},
		LocalPolicy:   "one_factor",
		PublicPolicy:  "two_factor",
	}})
	if n := strings.Count(content, "group:admins"); n != 2 {
		t.Errorf("both rules should carry the subject, got %d:\n%s", n, content)
	}
	if !strings.Contains(content, "policy: 'one_factor'") || !strings.Contains(content, "policy: 'two_factor'") {
		t.Errorf("both policies should appear:\n%s", content)
	}
}

// With no local networks resolved, only the catch-all is emitted — and it
// carries the public policy, so the local horizon gets the stricter treatment.
func TestAccessControl_NoLocalNetworksMeansSingleRule(t *testing.T) {
	_, content, _ := authelia{}.AccessControl([]Service{{
		Name: "media", FQDN: "media.example.com", Mode: ModeForward,
		PublicPolicy: "one_factor",
	}})
	if strings.Contains(content, "networks:") {
		t.Errorf("no networks rule expected:\n%s", content)
	}
	if n := strings.Count(content, "- domain: 'media.example.com'"); n != 1 {
		t.Errorf("expected exactly 1 rule, got %d:\n%s", n, content)
	}
}

// Path bypasses precede the local-horizon rule, so an exempt path stays exempt
// from every origin rather than only the public one.
func TestAccessControl_PathBypassesPrecedeLocalRule(t *testing.T) {
	_, content, _ := authelia{}.AccessControl([]Service{{
		Name: "gatus", FQDN: "status.example.com", Mode: ModeForward,
		BypassPaths:   []string{"/health"},
		LocalNetworks: []string{"10.0.0.0/8"},
		LocalPolicy:   "bypass",
		PublicPolicy:  "one_factor",
	}})
	iPath := strings.Index(content, "resources:")
	iNet := strings.Index(content, "networks:")
	if iPath == -1 || iNet == -1 || iPath > iNet {
		t.Errorf("path bypass must come before the networks rule:\n%s", content)
	}
}

// An unset PublicPolicy falls back to the provider's default gate, so existing
// services (which pass no policy at all) generate byte-identically.
func TestAccessControl_UnsetPolicyIsDefaultGate(t *testing.T) {
	_, content, _ := authelia{}.AccessControl([]Service{{
		Name: "x", FQDN: "x.example.com", Mode: ModeForward,
	}})
	if !strings.Contains(content, "policy: 'one_factor'") {
		t.Errorf("unset policy should render the default gate:\n%s", content)
	}
}
