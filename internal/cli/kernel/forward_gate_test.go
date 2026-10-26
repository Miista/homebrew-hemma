package kernel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --forward-gate (§4.9) is for an oidc service on the tunnel. add refuses it elsewhere before writing anything; update
// refuses it when asked for in the wrong combination, and clears a gate left over from before a change of mode or tunnel
// (the entry must not end up skipped).
func TestForwardGateFlag(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "defaults", "set", "tunnel-dir", "cloudflared"}); code != 0 {
		t.Fatalf("set tunnel-dir exit %d", code)
	}
	before, _ := os.ReadFile(filepath.Join(dir, configName))
	for _, args := range [][]string{
		{"--auth-mode", "oidc", "--forward-gate"},                // off the tunnel
		{"--auth-mode", "forward", "--tunnel", "--forward-gate"}, // forward is already asked on every request
		{"--tunnel", "--forward-gate"},                           // no auth mode
	} {
		cmd := append([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "docs:80"}, args...)
		if code := Run(cmd); code != 2 {
			t.Errorf("add %v: want exit 2, got %d", args, code)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, configName)); string(after) != string(before) {
		t.Fatalf("a refused add must not touch services.yaml")
	}

	if code := Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "docs:80",
		"--auth-mode", "oidc", "--tunnel", "--forward-gate"}); code != 0 {
		t.Fatalf("add oidc + tunnel + gate: exit %d", code)
	}
	if !mustLoad(t, dir).Services["docs"].Auth.ForwardGate {
		t.Fatal("the gate was not set")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy")); !strings.Contains(string(b), "import auth") {
		t.Errorf("a gated site imports auth (every request here: no local_networks set):\n%s", b)
	}

	// asked for in a combination it is not for: refused, nothing written
	before, _ = os.ReadFile(filepath.Join(dir, configName))
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--auth-mode", "forward", "--forward-gate"}); code != 2 {
		t.Errorf("update to forward with the gate: want exit 2, got %d", code)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, configName)); string(after) != string(before) {
		t.Errorf("a refused update must not touch services.yaml")
	}
	// left over: the tunnel goes, the gate goes with it
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--tunnel=false"}); code != 0 {
		t.Fatalf("update --tunnel=false: exit %d", code)
	}
	if svc := mustLoad(t, dir).Services["docs"]; svc.Auth.ForwardGate || svc.Tunnel {
		t.Errorf("turning the tunnel off must clear the gate: %+v", svc)
	}
}
