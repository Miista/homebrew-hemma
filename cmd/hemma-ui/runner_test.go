package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hemma/internal/cli/kernel"
	"hemma/internal/config"
)

// repoWithConfig builds a minimal valid hemma repo (synthetic data only).
func repoWithConfig(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	for _, h := range []string{"resolver", "appbox"} {
		if err := os.MkdirAll(filepath.Join(dir, h), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "hosts:\n  resolver: {ip: 192.0.2.1}\n  appbox: {ip: 192.0.2.2}\ndomains:\n  - example.com\ndefaults:\n  dns_host: resolver\nservices: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return &server{repo: dir}
}

// The UI runs commands through the kernel in-process: output is captured,
// mutations land in the repo, and failures come back as a non-zero code with
// the same message the CLI prints.
func TestRunKernel_InProcess(t *testing.T) {
	s := repoWithConfig(t)
	ctx := context.Background()

	code, out := s.runKernel(ctx, []string{"list", "--all"})
	if code != 0 || !strings.Contains(out, "== Hosts (2) ==") {
		t.Fatalf("list: code %d output %q", code, out)
	}

	code, out = s.runKernel(ctx, []string{"add", "service", "web", "--fqdn=web.example.com", "--host=appbox", "--backend=web:80"})
	if code != 0 {
		t.Fatalf("add service: code %d output %q", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(s.repo, "services.yaml")); !strings.Contains(string(b), "web.example.com") {
		t.Errorf("add service did not persist:\n%s", b)
	}

	code, out = s.runKernel(ctx, []string{"add", "service", "web", "--fqdn=web.example.com", "--host=appbox", "--backend=web:80"})
	if code == 0 || !strings.Contains(out, "Error:") {
		t.Errorf("duplicate add should fail with the CLI's message: code %d output %q", code, out)
	}
}

// A command that would open a terminal form refuses instead, whatever the
// server's own stdin is.
func TestRunKernel_NeverPrompts(t *testing.T) {
	s := repoWithConfig(t)
	code, out := s.runKernel(context.Background(), []string{"add", "service", "web"})
	if code == 0 || !strings.Contains(out, "stdin is not a terminal") {
		t.Errorf("want a refusal, got code %d output %q", code, out)
	}
}

// An unknown command word is refused, not run.
func TestRunKernel_UnknownCommand(t *testing.T) {
	s := repoWithConfig(t)
	if code, _ := s.runKernel(context.Background(), []string{"frobnicate"}); code == 0 {
		t.Error("unknown command must not succeed")
	}
}

// The request's deadline reaches external commands: a hung one is killed.
func TestRunKernel_DeadlineKillsExternalCommands(t *testing.T) {
	s := repoWithConfig(t)
	bin := t.TempDir()
	// A fake `dig` that hangs; verify on the resolver host would call docker,
	// so use measure's curl path instead: put a hanging `curl` first on PATH.
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "bash"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, _ := s.runKernel(ctx, []string{"measure", "https://example.com/"})
	if code == 0 {
		t.Error("a killed measure must fail")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("deadline did not stop the command: took %s", d)
	}
}

// /api/state carries what a deploy would do on THIS machine, from the kernel's own
// rules, so the confirmation dialog never promises a restart that will not happen.
func TestState_ReportsWhatDeployWouldDoHere(t *testing.T) {
	s := repoWithConfig(t)
	call := func() map[string]any {
		rec := httptest.NewRecorder()
		s.state(rec, httptest.NewRequest("GET", "/api/state", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("state: %v\n%s", err, rec.Body.String())
		}
		return out["apply"].(map[string]any)
	}

	// A machine that is no declared host: nothing to do, and the list is an empty array (never null).
	defer kernel.SetLocalHost(func(*config.Config) string { return "" })()
	if a := call(); a["host"] != "" || len(a["steps"].([]any)) != 0 {
		t.Errorf("unknown machine: %v", a)
	}

	// The resolver restarts pihole.
	kernel.SetLocalHost(func(*config.Config) string { return "resolver" })
	a := call()
	steps := a["steps"].([]any)
	if a["host"] != "resolver" || len(steps) != 1 || steps[0] != "restart pihole" {
		t.Errorf("resolver: %v", a)
	}
}
