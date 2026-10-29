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

// An update carries only what it changes (a role toggled on the Access page sends just the groups). The auth check judges the
// service as it would end up: the stored mode fills in what the request leaves out, so the same request is fine on a forward
// service and refused on one that does not go through the auth provider.
func TestRunCommand_UpdateChecksTheMergedService(t *testing.T) {
	s := repoWithConfig(t)
	post := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		s.runCommand(rec, httptest.NewRequest("POST", "/api/run", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	add := func(name, mode string) {
		t.Helper()
		b := `{"command":"add service","values":{"name":"` + name + `","fqdn":"` + name + `.example.com","host":"appbox","backend":"test:80","auth-mode":"` + mode + `"}}`
		if code, out := post(b); code != 200 {
			t.Fatalf("add %s: %d %s", name, code, out)
		}
	}
	add("gated", "forward")
	add("open", "none")

	if code, out := post(`{"command":"update service","values":{"name":"gated","auth-groups":"admins,family"}}`); code != 200 {
		t.Fatalf("groups only, forward service: want 200, got %d %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(s.repo, "services.yaml")); !strings.Contains(string(b), "- admins") || !strings.Contains(string(b), "- family") {
		t.Errorf("the groups were not written:\n%s", b)
	}
	// still as strict: groups on a service whose mode does not use the provider
	if code, out := post(`{"command":"update service","values":{"name":"open","auth-groups":"admins"}}`); code != 400 || !strings.Contains(out, "Auth groups need an auth mode") {
		t.Errorf("groups only, none service: want 400 with the reason, got %d %s", code, out)
	}
	// and stricter than before in one case: leaving the provider while the groups stay would be saved and then skipped
	if code, out := post(`{"command":"update service","values":{"name":"gated","auth-mode":"none"}}`); code != 400 || !strings.Contains(out, "Auth groups need an auth mode") {
		t.Errorf("mode none, groups left: want 400 with the reason, got %d %s", code, out)
	}
	// a present key wins, even an empty one: the form's way of clearing groups while it changes the mode
	if code, out := post(`{"command":"update service","values":{"name":"gated","auth-mode":"none","auth-groups":""}}`); code != 200 {
		t.Errorf("mode none, groups cleared: want 200, got %d %s", code, out)
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
	if r := post(`{"command":"verify","values":{}}`); r["changed"] != false {
		t.Errorf("a read-only command never changes anything: %v", r)
	}
}

// The recovery path: an unloadable services.yaml can still be read and replaced as text — through the kernel,
// and never with text that does not parse.
func TestRawConfig_ReadReplaceAndRefuse(t *testing.T) {
	s := repoWithConfig(t)
	path := filepath.Join(s.repo, "services.yaml")
	good, _ := os.ReadFile(path)

	save := func(text string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"text": text})
		rec := httptest.NewRecorder()
		s.handleRawSave(rec, httptest.NewRequest("POST", "/api/config/raw", strings.NewReader(string(b))))
		return rec
	}
	// While the file loads, it is not replaceable as text: the typed forms (and their checks) are the only way.
	if rec := save("hosts: {}\n"); rec.Code != 409 {
		t.Errorf("a loadable file must not be replaced as text: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	if now, _ := os.ReadFile(path); string(now) != string(good) {
		t.Errorf("the refusal must leave the file alone:\n%s", now)
	}

	// break it the way hemma once did: an auth mapping with no mode
	broken := strings.Replace(string(good), "services: {}", "services:\n  web:\n    fqdn: web.example.com\n    host: appbox\n    backend: web:80\n    auth:\n      groups: [admins]\n", 1)
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	s.handleRawGet(get, httptest.NewRequest("GET", "/api/config/raw", nil))
	var raw map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["text"] != broken || !strings.Contains(raw["error"].(string), "requires a mode") {
		t.Errorf("the broken file must be readable as text, with the reason: %v", raw)
	}
	state := httptest.NewRecorder()
	s.state(state, httptest.NewRequest("GET", "/api/state", nil))
	if !strings.Contains(state.Body.String(), "requires a mode") {
		t.Errorf("/api/state should report why it cannot load: %s", state.Body.String())
	}

	// still broken text: refused (400), file untouched
	before := s.configStamp()
	if rec := save("hosts: [unclosed"); rec.Code != 400 || s.configStamp() != before {
		t.Errorf("unparseable text: want 400 and no change, got %d changed=%v", rec.Code, s.configStamp() != before)
	}
	// fixed text: saved exactly, and everything loads again
	fixed := strings.Replace(broken, "      groups: [admins]", "      mode: forward\n      groups: [admins]", 1)
	if rec := save(fixed); rec.Code != 200 {
		t.Fatalf("fixed text: %d %s", rec.Code, rec.Body.String())
	}
	if now, _ := os.ReadFile(path); string(now) != fixed {
		t.Errorf("the text must be written as given:\n%s", now)
	}
	if code, out := s.runKernel(context.Background(), []string{"list", "--all"}); code != 0 || !strings.Contains(out, "web") {
		t.Errorf("after the rescue the UI's commands work again: %d\n%s", code, out)
	}
}

// A command that CHANGES something finishes even if the browser goes away mid-way (a phone locking during a
// deploy must not leave things half-done); a read-only one stops when the person leaves.
func TestRunCommand_ChangesFinishWhenTheBrowserLeaves_ReadsDoNot(t *testing.T) {
	s := repoWithConfig(t)
	setDeployHost(t, s, "resolver") // apply needs a deploy host (host.go)
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	// each external call takes a second and then records itself (absolute path: PATH holds only these fakes)
	slow := "#!/bin/sh\n/bin/sleep 1\necho \"$*\" >> \"$FAKE_LOG\"\n"
	// the read-only command's tool must BE the sleeper (exec), or killing it leaves an orphan holding the pipe open
	for tool, body := range map[string]string{"docker": slow, "bash": "#!/bin/sh\nexec /bin/sleep 5\n"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_LOG", logPath)
	defer kernel.SetLocalHost(func(*config.Config) string { return "resolver" })()
	if code, out := s.runKernel(context.Background(), []string{"doctor", "--fix"}); code > 1 {
		t.Fatalf("doctor --fix: %d\n%s", code, out)
	}

	run := func(body string, leaveAfter time.Duration) (*httptest.ResponseRecorder, time.Duration) {
		os.Remove(logPath)
		ctx, leave := context.WithCancel(context.Background()) // "the browser went away"
		time.AfterFunc(leaveAfter, leave)
		defer leave()
		req := httptest.NewRequest("POST", "/api/run", strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		start := time.Now()
		s.runCommand(rec, req)
		return rec, time.Since(start)
	}

	// apply restarts pihole (one slow docker call). The browser leaves 200ms in; the restart must still happen.
	rec, _ := run(`{"command":"apply","values":{}}`, 200*time.Millisecond)
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "restart pihole") {
		t.Errorf("apply was abandoned when the browser left: log %q, response %s", raw, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res["ok"] != true {
		t.Errorf("apply should have completed successfully: %s", rec.Body.String())
	}

	// measure is read-only (a slow `bash`): when the browser leaves it is stopped, not waited for. Its target is one
	// of the services, so there has to be one.
	if code, out := s.runKernel(context.Background(), []string{"add", "service", "web", "--fqdn=web.example.com", "--host=appbox", "--backend=web:80"}); code > 1 {
		t.Fatalf("add service: %d\n%s", code, out)
	}
	_, took := run(`{"command":"measure","values":{"target":"web"}}`, 200*time.Millisecond)
	if took > 800*time.Millisecond {
		t.Errorf("a read-only command should stop when the browser leaves; it ran %s", took)
	}
}

// The Issues page lists the findings `hemma doctor` makes, and marks one fixable ONLY if it carries the fixer `doctor --fix`
// runs. This runs the real doctor over real situations: the page's items are the CLI's findings (every title is in what
// `hemma doctor` prints), and what is fixable changes as --fix does its work.
func TestIssues_ListsDoctorsFindings_FixableOnlyWithAFixer(t *testing.T) {
	s := repoWithConfig(t)
	// no git on PATH: doctor then skips its gitignore check, which is fine here
	t.Setenv("PATH", t.TempDir())
	type item struct {
		Status  string `json:"status"`
		Title   string `json:"title"`
		Fixable bool   `json:"fixable"`
	}
	issues := func() (items []item, ok bool) {
		rec := httptest.NewRecorder()
		s.handleIssues(rec, httptest.NewRequest("GET", "/api/issues", nil))
		var j struct {
			OK    bool   `json:"ok"`
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatalf("%v\n%s", err, rec.Body.String())
		}
		return j.Items, j.OK
	}
	failing := func(items []item, title string) (found, fixable bool) {
		for _, it := range items {
			if (it.Status == "warn" || it.Status == "err") && strings.Contains(it.Title, title) {
				return true, it.Fixable
			}
		}
		return false, false
	}
	anyFixable := func(items []item) bool {
		for _, it := range items {
			if (it.Status == "warn" || it.Status == "err") && it.Fixable {
				return true
			}
		}
		return false
	}
	run := func(argv ...string) {
		t.Helper()
		if code, out := s.runKernel(context.Background(), argv); code > 1 {
			t.Fatalf("%v: %d\n%s", argv, code, out)
		}
	}

	// 1. brand new repo: generated files are missing → a fixable finding
	items, ok := issues()
	if found, fixable := failing(items, "out of sync"); ok || !found || !fixable {
		t.Errorf("missing files: ok=%v found=%v fixable=%v %+v", ok, found, fixable, items)
	}
	// the items are doctor's findings: each failing title is something `hemma doctor` prints
	_, direct := s.runKernel(context.Background(), []string{"doctor"})
	for _, it := range items {
		if (it.Status == "warn" || it.Status == "err") && !strings.Contains(direct, it.Title) {
			t.Errorf("the page lists %q, which `hemma doctor` does not print:\n%s", it.Title, direct)
		}
	}

	// 2. after --fix: the drift is gone and nothing is fixable — but what --fix cannot do is still listed (here: no
	// known_hosts entry for the deploy host, with its own "connect once interactively" step), without a fixer
	run("doctor", "--fix")
	items, ok = issues()
	if found, _ := failing(items, "out of sync"); found || ok || anyFixable(items) {
		t.Errorf("after --fix: ok=%v, nothing should be fixable and the drift gone: %+v", ok, items)
	}
	if found, fixable := failing(items, "known_hosts"); !found || fixable {
		t.Errorf("known_hosts should still be listed, unfixable: found=%v fixable=%v", found, fixable)
	}

	// 3. only advisories (an oidc service with no Authelia config): listed, but nothing fixable
	run("add", "service", "sso-app", "--fqdn=sso-app.example.com", "--host=appbox", "--backend=app:80", "--auth-mode=oidc")
	items, _ = issues()
	if found, fixable := failing(items, "OIDC"); !found || fixable {
		t.Errorf("an advisory is listed and is not fixable: found=%v fixable=%v %+v", found, fixable, items)
	}
}

// The measure target is one of the services — not free text. The server enforces it, whatever the form offered.
func TestMeasure_TargetMustBeAService(t *testing.T) {
	s := repoWithConfig(t)
	if code, out := s.runKernel(context.Background(), []string{"add", "service", "web", "--fqdn=web.example.com", "--host=appbox", "--backend=web:80"}); code > 1 {
		t.Fatalf("add service: %d\n%s", code, out)
	}
	for _, target := range []string{"https://evil.example.org/", "not-a-service", "web.example.com"} {
		rec := httptest.NewRecorder()
		s.runCommand(rec, httptest.NewRequest("POST", "/api/run", strings.NewReader(`{"command":"measure","values":{"target":"`+target+`"}}`)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "there is no service") {
			t.Errorf("target %q: want a 400 'there is no service', got %d %s", target, rec.Code, rec.Body.String())
		}
	}
	c := lookup("measure")
	var f *field
	for i := range c.Fields {
		if c.Fields[i].Name == "target" {
			f = &c.Fields[i]
		}
	}
	if f == nil || f.Ref != "service" || !f.Required {
		t.Errorf("the target must be a required service picker: %+v", f)
	}
}
