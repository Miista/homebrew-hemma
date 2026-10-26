package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// GateScope is the one rendering rule of §4.9: Caddy asks the provider only on a horizon where it has something to check.
func TestGateScope(t *testing.T) {
	pol := func(l, p PolicyLevel) AuthPolicy { return AuthPolicy{Local: l, Public: p} }
	for _, c := range []struct {
		name  string
		svc   Service
		local bool // local_networks configured
		want  GateScope
	}{
		{"none", Service{}, true, GateNone},
		{"external", Service{Auth: Auth{Mode: AuthExternal}}, true, GateNone},
		{"oidc", Service{Tunnel: true, Auth: Auth{Mode: AuthOIDC}}, true, GateNone},
		{"oidc + gate", Service{Tunnel: true, Auth: Auth{Mode: AuthOIDC, ForwardGate: true}}, true, GateOutside},
		{"oidc + gate, no local networks", Service{Tunnel: true, Auth: Auth{Mode: AuthOIDC, ForwardGate: true}}, false, GateAll},
		{"forward", Service{Tunnel: true, Auth: Auth{Mode: AuthForward}}, true, GateAll},
		{"forward, local two_factor", Service{Tunnel: true, Auth: Auth{Mode: AuthForward, Policy: pol(PolicyTwoFactor, PolicyTwoFactor)}}, true, GateAll},
		{"forward, local bypass", Service{Tunnel: true, Auth: Auth{Mode: AuthForward, Policy: pol(PolicyBypass, PolicyOneFactor)}}, true, GateOutside},
		{"forward, local bypass, no local networks", Service{Tunnel: true, Auth: Auth{Mode: AuthForward, Policy: pol(PolicyBypass, PolicyOneFactor)}}, false, GateAll},
		{"forward, bypass both", Service{Tunnel: true, Auth: Auth{Mode: AuthForward, Policy: pol(PolicyBypass, PolicyBypass)}}, true, GateNone},
		// off the tunnel an unset public follows local: bypass at home is bypass for everyone
		{"forward, local bypass, no tunnel", Service{Auth: Auth{Mode: AuthForward, Policy: pol(PolicyBypass, "")}}, true, GateNone},
	} {
		if got := c.svc.GateScope(c.local); got != c.want {
			t.Errorf("%s: GateScope = %d, want %d", c.name, got, c.want)
		}
	}
}

// forward_gate is read and written in the mapping form, and on its own is enough to need that form.
func TestForwardGate_RoundTrip(t *testing.T) {
	var a Auth
	if err := yaml.Unmarshal([]byte("{mode: oidc, forward_gate: true}"), &a); err != nil || a.Mode != AuthOIDC || !a.ForwardGate {
		t.Fatalf("read: %+v %v", a, err)
	}
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var back Auth
	if err := yaml.Unmarshal(out, &back); err != nil || !back.ForwardGate || back.Mode != AuthOIDC {
		t.Fatalf("written as %q, read back %+v %v", out, back, err)
	}
	if (Auth{Mode: AuthNone, ForwardGate: true}).IsZero() {
		t.Error("an auth with only forward_gate set is not empty")
	}
}
