package cli

import (
	"strings"
	"testing"

	"hemma/internal/cli/kernel"
)

// Every `defaults set` key (kernel.SetKeys) must have a matching help.go topic
// ("defaults set <key>") and a matching line in UsageText's defaults-commands
// block — these two are the parts that genuinely cannot be generated from the
// kernel's setSpecs (they're hand-written prose/formatting inside larger
// free-text blocks), so this test is the backstop that replaces generation for
// them. This is exactly the check that would have caught tunnel-dir missing
// from completion.go's set_keys before completion.go was made to generate from
// setSpecs instead.
func TestSetSpecs_HelpTopicsCoverEveryKey(t *testing.T) {
	topics := map[string]bool{}
	for _, h := range HelpTopics {
		topics[h.Cmd] = true
	}
	for _, key := range kernel.SetKeys() {
		wantCmd := "defaults set " + key
		if !topics[wantCmd] {
			t.Errorf("setSpecs key %q has no matching HelpTopics entry %q", key, wantCmd)
		}
	}
}

// Every setSpecs entry must appear in UsageText's top-level listing — the one
// place that stays hand-written (it's one line among many other hand-written
// lines in a single large const block, so generating just the set lines would
// make the block inconsistent with itself).
func TestSetSpecs_UsageTextCoversEveryKey(t *testing.T) {
	for _, key := range kernel.SetKeys() {
		want := "hemma defaults set " + key
		if !strings.Contains(UsageText, want) {
			t.Errorf("UsageText is missing a line for setSpecs key %q (want it to contain %q)", key, want)
		}
	}
}

// The generated completion scripts must offer every setSpecs key and nothing
// else — this is now enforced by construction (both scripts interpolate
// kernel.SetKeys() directly), but pinning it as a test means a future refactor
// that reintroduces a hardcoded list fails immediately instead of silently.
func TestSetSpecs_CompletionScriptsCoverEveryKey(t *testing.T) {
	for _, key := range kernel.SetKeys() {
		if !strings.Contains(BashCompletion, key) {
			t.Errorf("BashCompletion is missing setSpecs key %q", key)
		}
		if !strings.Contains(ZshCompletion, key) {
			t.Errorf("ZshCompletion is missing setSpecs key %q", key)
		}
	}
}
