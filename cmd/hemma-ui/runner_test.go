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

// The server refuses an auth combination hemma would save and then skip — whatever the form offered.
func TestCheckAuthFields(t *testing.T) {
	add := lookup("add service")
	cases := []struct {
		name string
		vals map[string]any
		bad  bool
	}{
		{"nothing set", map[string]any{"auth-mode": ""}, false},
		{"none + bypass", map[string]any{"auth-mode": "none", "bypass-paths": "/api"}, true},
		{"unset mode + groups", map[string]any{"auth-groups": "admins"}, true},
		{"external + groups", map[string]any{"auth-mode": "external", "auth-groups": "admins"}, true},
		{"forward + groups + bypass", map[string]any{"auth-mode": "forward", "auth-groups": "admins", "bypass-paths": "/api"}, false},
		{"oidc + groups", map[string]any{"auth-mode": "oidc", "auth-groups": "admins"}, false},
		{"oidc + policy", map[string]any{"auth-mode": "oidc", "auth-policy-local": "bypass"}, true},
		{"forward + policy", map[string]any{"auth-mode": "forward", "auth-policy-public": "one_factor"}, false},
		{"none + policy", map[string]any{"auth-mode": "none", "auth-policy-local": "bypass"}, true},
	}
	for _, c := range cases {
		if err := checkAuthFields(add, c.vals); (err != nil) != c.bad {
			t.Errorf("%s: err=%v, want refused=%v", c.name, err, c.bad)
		}
	}
	// Other commands are not its business.
	if err := checkAuthFields(lookup("add host"), map[string]any{"auth-groups": "x"}); err != nil {
		t.Errorf("add host: %v", err)
	}
}

// A refused add is refused BEFORE anything is saved; a combination the form never offers cannot be
// smuggled in through the API.
func TestRunCommand_RefusesBadAuthComboWithoutSaving(t *testing.T) {
	s := repoWithConfig(t)
	before := s.configStamp()
	body := `{"command":"add service","values":{"name":"lol","fqdn":"lol.example.com","host":"appbox","backend":"test:80","auth-mode":"","bypass-paths":"/api"}}`
	rec := httptest.NewRecorder()
	s.runCommand(rec, httptest.NewRequest("POST", "/api/run", strings.NewReader(body)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Bypass paths need an auth mode") {
		t.Fatalf("want 400 with the reason, got %d %s", rec.Code, rec.Body.String())
	}
	if s.configStamp() != before {
		t.Error("services.yaml changed although the request was refused")
	}
}

// The response says whether services.yaml moved, so the page can tell "refused" from "saved, with problems".
func TestRunCommand_ReportsWhetherTheConfigChanged(t *testing.T) {
	s := repoWithConfig(t)
	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		s.runCommand(rec, httptest.NewRequest("POST", "/api/run", strings.NewReader(body)))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v\n%s", err, rec.Body.String())
		}
		return out
	}
	add := `{"command":"add service","values":{"name":"web","fqdn":"web.example.com","host":"appbox","backend":"web:80"}}`
	if r := post(add); r["ok"] != true || r["changed"] != true {
		t.Errorf("a successful add: %v", r)
	}
	// The same add again is refused by hemma (exists): a non-zero exit with NOTHING saved.
	if r := post(add); r["ok"] == true || r["changed"] != false {
		t.Errorf("a refused add must report changed=false: %v", r)
	}
	if r := post(`{"command":"list","values":{}}`); r["changed"] != false {
		t.Errorf("a read-only command never changes anything: %v", r)
	}
}
