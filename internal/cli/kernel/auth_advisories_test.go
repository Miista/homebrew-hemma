package kernel

import (
	"strings"
	"testing"

	"hemma/internal/config"
)

// A provider-backed service whose effective policy bypasses both horizons is mode none behind a padlock: doctor (and every
// other caller of authConfigWarnings) says so, once per service. Bypass at home only is deliberate, a disabled service is not
// served, and without the tunnel an unset public follows local — so a local-only `local: bypass` is flagged too.
func TestAuthConfigWarnings_BypassEverywhere(t *testing.T) {
	pol := func(l, p config.PolicyLevel) config.Auth {
		return config.Auth{Mode: config.AuthForward, Policy: config.AuthPolicy{Local: l, Public: p}}
	}
	cfg := &config.Config{Services: map[string]config.Service{
		"home":     {Tunnel: true, Auth: pol(config.PolicyBypass, config.PolicyOneFactor)},
		"both":     {Tunnel: true, Auth: pol(config.PolicyBypass, config.PolicyBypass)},
		"homeonly": {Auth: pol(config.PolicyBypass, "")},
		"off":      {Disabled: true, Auth: pol(config.PolicyBypass, config.PolicyBypass)},
		"plain":    {Auth: config.Auth{Mode: config.AuthForward}},
	}}
	var flagged []string
	for _, a := range authConfigWarnings(t.TempDir(), cfg) {
		if strings.Contains(a.Headline, "bypass on both horizons") {
			flagged = append(flagged, strings.Fields(a.Headline)[3])
		}
	}
	if got := strings.Join(flagged, ","); got != "both,homeonly" {
		t.Errorf("flagged %q, want both,homeonly", got)
	}
}
