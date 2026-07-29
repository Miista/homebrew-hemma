package cli

import (
	"strings"
	"testing"
)

// Every setSpecs entry must have a matching help.go topic ("set <key>") and a
// matching line in UsageText's set-commands block — these two are the parts
// that genuinely cannot be generated from setSpecs (they're hand-written
// prose/formatting inside larger free-text blocks), so this test is the
// backstop that replaces generation for them. This is exactly the check that
// would have caught tunnel-dir missing from completion.go's set_keys before
// completion.go was made to generate from setSpecs instead.
func TestSetSpecs_HelpTopicsCoverEveryKey(t *testing.T) {
	topics := map[string]bool{}
	for _, h := range HelpTopics {
		topics[h.Cmd] = true
	}
	for _, s := range setSpecs {
		wantCmd := "set " + string(s.key)
		if !topics[wantCmd] {
			t.Errorf("setSpecs key %q has no matching HelpTopics entry %q", s.key, wantCmd)
		}
	}
}

// Every setSpecs entry must appear in UsageText's top-level listing — the one
// place that stays hand-written (it's one line among many other hand-written
// lines in a single large const block, so generating just the set lines would
// make the block inconsistent with itself).
func TestSetSpecs_UsageTextCoversEveryKey(t *testing.T) {
	for _, s := range setSpecs {
		want := "hemma set    " + string(s.key)
		if !strings.Contains(UsageText, want) {
			t.Errorf("UsageText is missing a line for setSpecs key %q (want it to contain %q)", s.key, want)
		}
	}
}

// The generated completion scripts must offer every setSpecs key and nothing
// else — this is now enforced by construction (both scripts interpolate
// setKeyStrings() directly), but pinning it as a test means a future refactor
// that reintroduces a hardcoded list fails immediately instead of silently.
func TestSetSpecs_CompletionScriptsCoverEveryKey(t *testing.T) {
	for _, s := range setSpecs {
		if !strings.Contains(BashCompletion, string(s.key)) {
			t.Errorf("BashCompletion is missing setSpecs key %q", s.key)
		}
		if !strings.Contains(ZshCompletion, string(s.key)) {
			t.Errorf("ZshCompletion is missing setSpecs key %q", s.key)
		}
	}
}

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
			args = []string{"set", string(s.key), "resolver"}
			if s.key == SetAuthService {
				// resolver is a host, not a service — expect a clean rejection
				// (exit 1, not the "Unknown setting" exit-2 dispatch failure).
				args = []string{"set", string(s.key), "ghost-service"}
			}
		case SetAuthSnippet:
			args = []string{"set", string(s.key), "-"}
		case SetTunnelDir:
			args = []string{"set", string(s.key), "appbox", "-"}
		default:
			t.Fatalf("unhandled SetKey %q in test — add a case above", s.key)
		}
		code := Run(append([]string{"-C", dir}, args...))
		// 2 is dispatchSet's own "missing arg" / "unknown setting" exit code;
		// anything else means the key was recognized and routed.
		if code == 2 {
			out := captureStderr(t, func() { Run(append([]string{"-C", dir}, args...)) })
			if strings.Contains(out, "Unknown setting") {
				t.Errorf("dispatchSet did not recognize setSpecs key %q: %s", s.key, out)
			}
		}
	}
}
