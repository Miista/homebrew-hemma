package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wiringSvcs is a service set for which AccessControl emits both an
// access_control section (forward) and an oidc policy (oidc-with-groups).
var wiringSvcs = []Service{
	{Name: "pihole", FQDN: "pihole.example.com", Mode: ModeForward, Groups: []string{"admins"}},
	{Name: "grafana", FQDN: "grafana.example.com", Mode: ModeOIDC, Groups: []string{"admins"}},
}

// A secret-bearing env value that must never leak into warnings.
const composeSecret = "hunter2-oidc-hmac-secret"

// writeWiringFixture creates a host dir with the given docker-compose.yml and
// (optionally) an Authelia configuration.yml, returning the host dir.
func writeWiringFixture(t *testing.T, composeYAML, configYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if composeYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if configYAML != "" {
		cfgDir := filepath.Join(dir, "authelia", "data", "config")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "configuration.yml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// joinAdvisories flattens advisories to plain text for substring assertions.
func joinAdvisories(advs []Advisory) string {
	var parts []string
	for _, a := range advs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, "\n")
}

func assertNoSecretLeak(t *testing.T, advs []Advisory) {
	t.Helper()
	if strings.Contains(joinAdvisories(advs), composeSecret) {
		t.Errorf("advisories must never quote compose content beyond X_AUTHELIA_CONFIG:\n%s", joinAdvisories(advs))
	}
}

func TestValidateWiring_EnvMapMissingEntry(t *testing.T) {
	compose := `services:
  authelia:
    image: authelia/authelia
    environment:
      AUTHELIA_IDENTITY_PROVIDERS_OIDC_HMAC_SECRET: ` + composeSecret + `
      X_AUTHELIA_CONFIG: /config/configuration.yml
`
	dir := writeWiringFixture(t, compose, "")
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 {
		t.Fatalf("want 1 warning, got %d: %v", len(w), w)
	}
	// The advisory must diagnose (quoting the current value) AND carry the
	// exact value to paste in, preserving the existing entries. Consequence in
	// the headline; container paths stay container paths.
	if !strings.Contains(w[0].Headline, "not enforced") {
		t.Errorf("headline should state the consequence:\n%s", w[0])
	}
	if !strings.Contains(w[0].String(), `X_AUTHELIA_CONFIG="/config/configuration.yml"`) {
		t.Errorf("advisory should quote the current env value:\n%s", w[0])
	}
	if !strings.Contains(w[0].String(), "X_AUTHELIA_CONFIG: '/config/configuration.yml,/config/hemma.access_control.generated.yml'") {
		t.Errorf("advisory should carry the exact env line to set:\n%s", w[0])
	}
	assertNoSecretLeak(t, w)
}

func TestValidateWiring_EnvListWired(t *testing.T) {
	compose := `services:
  auth:
    container_name: authelia
    environment:
      - SECRET=` + composeSecret + `
      - X_AUTHELIA_CONFIG=/config/configuration.yml, /config/hemma.access_control.generated.yml
`
	dir := writeWiringFixture(t, compose, "")
	// Matched via container_name, not the service key; entry found despite
	// the space after the comma.
	if w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs); w != nil {
		t.Errorf("correctly wired list-form env must be silent, got %v", w)
	}
}

func TestValidateWiring_EnvVarAbsent(t *testing.T) {
	compose := `services:
  authelia:
    image: authelia/authelia
`
	dir := writeWiringFixture(t, compose, "")
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 || !strings.Contains(w[0].String(), "X_AUTHELIA_CONFIG is not set") {
		t.Fatalf("want one not-set advisory, got %v", w)
	}
	// Absent var -> the conventional default value is suggested.
	if !strings.Contains(w[0].String(), "X_AUTHELIA_CONFIG: '/config/configuration.yml,/config/hemma.access_control.generated.yml'") {
		t.Errorf("advisory should carry the conventional env line:\n%s", w[0])
	}
	if w[0].Then != "hemma apply" {
		t.Errorf("advisory should say how to make it live (then: hemma apply):\n%s", w[0])
	}
}

// public_paths exemptions are enforced ONLY by the artifact's bypass rules
// (Caddy renders no per-path branches), so the unwired advisory must call out
// the consequence — the declared public paths stay auth-gated — folded into
// the ONE existing advisory, listing the paths.
func TestValidateWiring_UnwiredListsGatedPublicPaths(t *testing.T) {
	compose := `services:
  authelia:
    image: authelia/authelia
`
	dir := writeWiringFixture(t, compose, "")
	svcs := []Service{
		{Name: "status", FQDN: "status.example.com", Mode: ModeForward, BypassPaths: []string{"/health", "/api/badges/*"}},
		{Name: "pihole", FQDN: "pihole.example.com", Mode: ModeForward, Groups: []string{"admins"}},
	}
	w := (authelia{}).ValidateWiring(dir, "authelia", svcs)
	if len(w) != 1 {
		t.Fatalf("consequence must fold into the single unwired advisory, got %d: %v", len(w), w)
	}
	s := w[0].String()
	for _, want := range []string{"auth.bypass_paths are NOT exempt",
		"status.example.com/health", "status.example.com/api/badges/*"} {
		if !strings.Contains(s, want) {
			t.Errorf("advisory should contain %q:\n%s", want, s)
		}
	}
	// Still carries the paste-in fix (house style: advisories always do).
	if !strings.Contains(s, "X_AUTHELIA_CONFIG: '/config/configuration.yml,/config/hemma.access_control.generated.yml'") {
		t.Errorf("advisory lost the paste-in env line:\n%s", s)
	}

	// No public_paths declared → no consequence line.
	w = (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 || strings.Contains(w[0].String(), "bypass_paths") {
		t.Errorf("no public_paths → no consequence line, got %v", w)
	}
}

func TestValidateWiring_HandWrittenAccessControlConflict(t *testing.T) {
	compose := `services:
  authelia:
    environment:
      X_AUTHELIA_CONFIG: /config/configuration.yml,/config/hemma.access_control.generated.yml
`
	config := `access_control:
  default_policy: deny
  rules:
    - domain: old.example.com
      policy: one_factor
`
	dir := writeWiringFixture(t, compose, config)
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 {
		t.Fatalf("want 1 duplicate warning (wiring itself is correct), got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0].String(), "does not merge rule lists") || !strings.Contains(w[0].String(), "remove the access_control section from") {
		t.Errorf("want duplicate-section advisory with the concrete fix:\n%s", w[0])
	}
	// The hand-written rules themselves must not be quoted.
	if strings.Contains(w[0].String(), "old.example.com") {
		t.Errorf("advisory must not quote configuration.yml content:\n%s", w[0])
	}
}

func TestValidateWiring_NoConflictWhenArtifactHasNoAccessControl(t *testing.T) {
	// oidc-with-groups only: the artifact renders authorization_policies but
	// no access_control section, so a hand-written access_control is fine.
	compose := `services:
  authelia:
    environment:
      X_AUTHELIA_CONFIG: /config/configuration.yml,/config/hemma.access_control.generated.yml
`
	config := "access_control:\n  default_policy: deny\n"
	dir := writeWiringFixture(t, compose, config)
	oidcOnly := []Service{{Name: "grafana", FQDN: "g.example.com", Mode: ModeOIDC, Groups: []string{"admins"}}}
	if w := (authelia{}).ValidateWiring(dir, "authelia", oidcOnly); w != nil {
		t.Errorf("no access_control in artifact -> no duplicate warning, got %v", w)
	}
}

func TestValidateWiring_MissingComposeSoftAdvisory(t *testing.T) {
	dir := writeWiringFixture(t, "", "")
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 || !strings.Contains(w[0].Headline, "could not verify") {
		t.Fatalf("want one could-not-verify advisory, got %v", w)
	}
	// Soft advisories are headline-only: no fix to give.
	if len(w[0].Fix) != 0 {
		t.Errorf("could-not-verify advisory should carry no fix, got %v", w[0].Fix)
	}
}

func TestValidateWiring_UnparseableComposeSoftAdvisory(t *testing.T) {
	dir := writeWiringFixture(t, "services: [not a map\n", "")
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 || !strings.Contains(w[0].Headline, "could not verify") {
		t.Fatalf("want one could-not-verify advisory, got %v", w)
	}
}

func TestValidateWiring_ServiceNotInComposeSoftAdvisory(t *testing.T) {
	dir := writeWiringFixture(t, "services:\n  caddy:\n    image: caddy\n", "")
	w := (authelia{}).ValidateWiring(dir, "authelia", wiringSvcs)
	if len(w) != 1 || !strings.Contains(w[0].Headline, "could not verify") || !strings.Contains(w[0].Headline, `no service "authelia"`) {
		t.Fatalf("want one could-not-verify advisory naming the missing service, got %v", w)
	}
}

func TestValidateWiring_NoArtifactNoOutput(t *testing.T) {
	// No forward and no oidc-with-groups services: AccessControl emits
	// nothing, so there is nothing to wire — silent even without a compose.
	dir := writeWiringFixture(t, "", "")
	svcs := []Service{{Name: "grafana", FQDN: "g.example.com", Mode: ModeOIDC}}
	if w := (authelia{}).ValidateWiring(dir, "authelia", svcs); w != nil {
		t.Errorf("no artifact planned -> no output, got %v", w)
	}
}
