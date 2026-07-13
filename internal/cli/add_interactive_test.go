package cli

import (
	"reflect"
	"strings"
	"testing"

	"hemma/internal/config"
)

// Dispatch: `add service <name>` with zero flags is the interactive editor,
// TTY-gated. Under `go test` stdin is not a terminal, so it must refuse with
// exit 2 and point at the flags — never hang on a hidden prompt.
func TestRun_AddZeroFlagsNonTTYRefused(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)

	var code int
	out := captureStderr(t, func() {
		code = Run([]string{"-C", dir, "add", "service", "docs"})
	})
	if code != 2 {
		t.Errorf("zero-flag add on non-TTY stdin should exit 2, got %d", code)
	}
	if !strings.Contains(out, "stdin is not a terminal") {
		t.Errorf("refusal should explain the TTY gate, got: %q", out)
	}
}

// Dispatch: any flag present keeps the existing non-interactive path,
// unchanged (no TTY needed).
func TestRun_AddWithFlagsStaysNonInteractive(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)

	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 0 {
		t.Fatalf("flag add should exit 0 without a TTY, got %d", code)
	}
	cfg, err := config.Load(dir + "/" + configName)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services["docs"].Backend; got != "paperless:8000" {
		t.Errorf("service not added via flags path: %q", got)
	}
}

// A partial flag set is a usage error on the flags path, not a fall-through
// to the editor — forgetting one flag must not open a hidden prompt.
func TestRun_AddPartialFlagsIsUsageError(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)

	var code int
	out := captureStderr(t, func() {
		code = Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com"})
	})
	if code != 2 {
		t.Errorf("partial-flag add should exit 2, got %d", code)
	}
	if !strings.Contains(out, "--host") || !strings.Contains(out, "--backend") {
		t.Errorf("error should name the missing flags, got: %q", out)
	}
}

// The submit summary lists the collected fields; auth lines only when a gate
// is set.
func TestSummarizeNewService(t *testing.T) {
	plain := config.Service{FQDN: "a.example.com", Host: "appbox", Backend: "a:80"}
	want := []string{
		"  fqdn: a.example.com",
		"  host: appbox",
		"  backend: a:80",
	}
	if got := summarizeNewService(plain); !reflect.DeepEqual(got, want) {
		t.Errorf("summary mismatch:\n got  %v\n want %v", got, want)
	}

	gated := plain
	gated.Auth = config.Auth{Mode: config.AuthForward, Groups: []string{"family", "admins"}}
	want = append(want,
		"  auth mode: forward",
		"  auth groups: admins, family",
	)
	if got := summarizeNewService(gated); !reflect.DeepEqual(got, want) {
		t.Errorf("summary mismatch:\n got  %v\n want %v", got, want)
	}
}
