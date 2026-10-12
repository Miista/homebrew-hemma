package cli

import (
	"strings"
	"testing"
)

// The completion command prints a script for bash/zsh and rejects others.
func TestRun_Completion(t *testing.T) {
	run := func(args ...string) (int, string) {
		code, stdout, _ := runCLI(t, args)
		return code, stdout
	}
	code, bash := run("completion", "bash")
	if code != 0 {
		t.Fatalf("completion bash exit %d", code)
	}
	if !strings.Contains(bash, "complete -F _hemma hemma") {
		t.Errorf("bash script missing complete registration:\n%s", bash)
	}
	code, zsh := run("completion", "zsh")
	if code != 0 {
		t.Fatalf("completion zsh exit %d", code)
	}
	if !strings.Contains(zsh, "#compdef hemma") {
		t.Errorf("zsh script missing #compdef header:\n%s", zsh)
	}
	if code, _ := run("completion", "fish"); code != 2 {
		t.Errorf("unsupported shell should exit 2, got %d", code)
	}
	if code, _ := run("completion"); code != 2 {
		t.Errorf("missing shell arg should exit 2, got %d", code)
	}
}
