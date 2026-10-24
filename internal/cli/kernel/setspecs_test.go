package kernel

import (
	"strings"
	"testing"
)

// (The cross-checks of setSpecs against help topics, UsageText and the
// completion scripts live in package cli, which owns those.)

// dispatchSet must recognize every setSpecs key (i.e. never fall through to
// "Unknown setting") and route to a working handler.
func TestSetSpecs_DispatchRecognizesEveryKey(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	for _, s := range setSpecs {
		var args []string
		switch s.key {
		case SetDNSHost, SetDeployHost, SetAuthService:
			args = []string{"defaults", "set", string(s.key), "resolver"}
			if s.key == SetAuthService {
				// resolver is a host, not a service — expect a clean rejection
				// (exit 1, not the "Unknown setting" exit-2 dispatch failure).
				args = []string{"defaults", "set", string(s.key), "ghost-service"}
			}
		case SetAuthSnippet:
			args = []string{"defaults", "set", string(s.key), "-"}
		case SetTunnelDir, SetLocalNetworks:
			args = []string{"defaults", "set", string(s.key), "-"}
		default:
			t.Fatalf("unhandled SetKey %q in test — add a case above", s.key)
		}
		code := Run(append([]string{"-C", dir}, args...))
		// 2 is dispatchDefaults' own "missing arg" / "unknown setting" exit
		// code; anything else means the key was recognized and routed.
		if code == 2 {
			out := captureStderr(t, func() { Run(append([]string{"-C", dir}, args...)) })
			if strings.Contains(out, "Unknown setting") {
				t.Errorf("dispatchDefaults did not recognize setSpecs key %q: %s", s.key, out)
			}
		}
	}
}

// `hemma defaults` (bare) prints every setSpecs key, "(unset)" for anything
// not configured, and the real value for anything that is.
func TestDefaultsShow_ListsEveryKeyWithCurrentValue(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir) // seed sets dns_host: resolver; everything else starts unset
	if code := Run([]string{"-C", dir, "defaults", "set", "tunnel-dir", "cloudflared"}); code != 0 {
		t.Fatalf("set tunnel-dir exit %d", code)
	}
	out := captureStdout(t, func() {
		if code := Run([]string{"-C", dir, "defaults"}); code != 0 {
			t.Errorf("bare 'defaults' should exit 0, got %d", code)
		}
	})
	for _, s := range setSpecs {
		if !strings.Contains(out, string(s.key)) {
			t.Errorf("defaults output missing key %q:\n%s", s.key, out)
		}
	}
	if !strings.Contains(out, "resolver") {
		t.Errorf("dns-host's current value 'resolver' should be printed:\n%s", out)
	}
	if !strings.Contains(out, "cloudflared") {
		t.Errorf("tunnel-dir's current value 'cloudflared' should be printed:\n%s", out)
	}
	if !strings.Contains(out, "(unset)") {
		t.Errorf("an unset key (e.g. auth-service) should show (unset):\n%s", out)
	}
}
