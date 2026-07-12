package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// deployCfg builds an in-memory config: resolver is the dns_host, appbox runs
// a service, spare has no role.
func deployCfg() *config.Config {
	return &config.Config{
		Hosts: map[string]config.Host{
			"resolver": {IP: "192.0.2.1"},
			"appbox":   {IP: "192.0.2.2", SSH: "admin@appbox.lan"},
			"spare":    {IP: "192.0.2.9"},
		},
		Domains:  map[string]config.Domain{"example.com": {}},
		Defaults: config.Defaults{DNSHost: "resolver"},
		Services: map[string]config.Service{
			"docs": {FQDN: "docs.example.com", Host: "appbox", Backend: "x:1"},
		},
	}
}

func targetNames(ts []deployTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// Default targets: every host with a role (service host or dns_host) —
// role-less hosts are excluded — and self is ordered LAST.
func TestResolveDeployTargets_RolesAndSelfLast(t *testing.T) {
	cfg := deployCfg()
	ts, err := resolveDeployTargets(cfg, "resolver", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(targetNames(ts), ","); got != "appbox,resolver" {
		t.Errorf("want appbox,resolver (self last, spare excluded), got %s", got)
	}
	if !ts[1].Local || ts[0].Local {
		t.Errorf("only self (resolver) should be Local: %+v", ts)
	}
	// The ssh field is the verbatim destination; absent it defaults to the name.
	if ts[0].Dest != "admin@appbox.lan" {
		t.Errorf("appbox dest should come from the ssh field, got %q", ts[0].Dest)
	}
	// No self among the hosts → nothing Local, plain sorted order.
	ts, err = resolveDeployTargets(cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(targetNames(ts), ","); got != "appbox,resolver" {
		t.Errorf("want appbox,resolver, got %s", got)
	}
	for _, tt := range ts {
		if tt.Local {
			t.Errorf("no target should be Local when self is unknown: %+v", tt)
		}
	}
}

// Explicit names restrict the set (even to role-less hosts), dedupe, default
// the ssh destination to the name, and refuse unknown names.
func TestResolveDeployTargets_ExplicitNames(t *testing.T) {
	cfg := deployCfg()
	ts, err := resolveDeployTargets(cfg, "appbox", []string{"spare", "appbox", "spare"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(targetNames(ts), ","); got != "spare,appbox" {
		t.Errorf("want spare,appbox (deduped, self last), got %s", got)
	}
	if ts[0].Dest != "spare" {
		t.Errorf("dest should default to the host name, got %q", ts[0].Dest)
	}
	if _, err := resolveDeployTargets(cfg, "", []string{"ghost"}); err == nil {
		t.Error("unknown host name should be refused")
	}
}

// fakeRunner records every command and fails/answers per script. Keys are
// "<host> <first two argv words>"-ish prefixes matched against the joined argv.
type fakeRunner struct {
	calls    []string          // "<host>: <argv joined>"
	failOn   map[string]bool   // host -> fail its pull
	applyErr map[string]bool   // host -> fail its apply
	heads    map[string]string // host -> rev-parse output (default "aaa")
}

func (f *fakeRunner) run(t deployTarget, argv []string) (string, error) {
	cmd := strings.Join(argv, " ")
	f.calls = append(f.calls, t.Name+": "+cmd)
	switch {
	case strings.Contains(cmd, "pull"):
		if f.failOn[t.Name] {
			return "fatal: Not possible to fast-forward, aborting.", fmt.Errorf("exit status 128")
		}
		return "Already up to date.", nil
	case strings.Contains(cmd, "rev-parse"):
		if h, ok := f.heads[t.Name]; ok {
			return h + "\n", nil
		}
		return "aaa\n", nil
	case strings.Contains(cmd, "apply"):
		if f.applyErr[t.Name] {
			return "boom", fmt.Errorf("exit status 1")
		}
		return "Applied.", nil
	}
	return "", nil
}

func (f *fakeRunner) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

var deployTestTargets = []deployTarget{
	{Name: "appbox", Dest: "admin@appbox.lan"},
	{Name: "resolver", Dest: "resolver", Local: true},
}

func TestRunDeploy_HappyPath(t *testing.T) {
	f := &fakeRunner{}
	if code := runDeploy(f, deployTestTargets, "/repo"); code != 0 {
		t.Fatalf("happy path should exit 0, got %d (calls: %v)", code, f.calls)
	}
	// Remote pulls the convention path, local pulls the local checkout.
	wantPrefix := []string{
		"appbox: git -C ~/docker pull --ff-only",
		"resolver: git -C /repo pull --ff-only",
	}
	for i, w := range wantPrefix {
		if f.calls[i] != w {
			t.Errorf("call %d: want %q, got %q", i, w, f.calls[i])
		}
	}
	// Apply fans out remotes first, self last, and self runs this binary.
	applies := []string{}
	for _, c := range f.calls {
		if strings.Contains(c, "apply") {
			applies = append(applies, c)
		}
	}
	if len(applies) != 2 || !strings.HasPrefix(applies[0], "appbox: hemma apply") {
		t.Errorf("remote apply should come first as 'hemma apply', got %v", applies)
	}
	if !strings.HasPrefix(applies[1], "resolver: ") || !strings.HasSuffix(applies[1], "-C /repo apply") {
		t.Errorf("self apply should run last against the local checkout, got %v", applies)
	}
}

// Phase 1 abort: a pull failure anywhere stops the whole deploy — later hosts
// are not pulled and NOTHING is applied.
func TestRunDeploy_Phase1AbortOnPullFailure(t *testing.T) {
	f := &fakeRunner{failOn: map[string]bool{"appbox": true}}
	if code := runDeploy(f, deployTestTargets, "/repo"); code != 1 {
		t.Fatalf("pull failure should exit 1, got %d", code)
	}
	if f.count("pull") != 1 {
		t.Errorf("no further pulls after a failure, got calls %v", f.calls)
	}
	if f.count("apply") != 0 {
		t.Errorf("phase 2 must never start after a pull failure, got calls %v", f.calls)
	}
}

// Phase 1 abort: hosts landing on different commits (a racing push) also
// aborts before anything is applied.
func TestRunDeploy_Phase1AbortOnHeadMismatch(t *testing.T) {
	f := &fakeRunner{heads: map[string]string{"appbox": "aaa", "resolver": "bbb"}}
	if code := runDeploy(f, deployTestTargets, "/repo"); code != 1 {
		t.Fatalf("head mismatch should exit 1, got %d", code)
	}
	if f.count("apply") != 0 {
		t.Errorf("phase 2 must never start on a head mismatch, got calls %v", f.calls)
	}
}

// Phase 2 is best-effort: an apply failure on one host is reported, the
// remaining hosts still apply, and the exit code is non-zero.
func TestRunDeploy_ApplyFailureContinues(t *testing.T) {
	f := &fakeRunner{applyErr: map[string]bool{"appbox": true}}
	if code := runDeploy(f, deployTestTargets, "/repo"); code != 1 {
		t.Fatalf("apply failure should exit 1, got %d", code)
	}
	if f.count("apply") != 2 {
		t.Errorf("apply must continue to remaining hosts, got calls %v", f.calls)
	}
}

// --- preflight: deploy refuses a dirty or unpushed local repo ---

// gitRepo creates origin (bare) + a pushed clone and returns the clone path.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--bare", "-b", "main", origin)
	git("clone", origin, clone)
	if err := os.WriteFile(filepath.Join(clone, "f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", clone, "add", "f")
	git("-C", clone, "commit", "-m", "init")
	git("-C", clone, "push", "-u", "origin", "main")
	return clone
}

func TestDeployPreflight(t *testing.T) {
	clone := gitRepo(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", clone}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Clean and pushed → allowed.
	if err := deployPreflight(clone); err != nil {
		t.Errorf("clean pushed repo should pass preflight: %v", err)
	}

	// Dirty working tree → refused.
	if err := os.WriteFile(filepath.Join(clone, "f"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deployPreflight(clone); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Errorf("dirty repo should be refused, got %v", err)
	}

	// Committed but unpushed → refused.
	git("add", "f")
	git("commit", "-m", "local only")
	if err := deployPreflight(clone); err == nil || !strings.Contains(err.Error(), "unpushed") {
		t.Errorf("unpushed commit should be refused, got %v", err)
	}

	// Pushed → allowed again.
	git("push")
	if err := deployPreflight(clone); err != nil {
		t.Errorf("pushed repo should pass preflight: %v", err)
	}

	// Not a git repo at all → refused.
	if err := deployPreflight(t.TempDir()); err == nil {
		t.Error("a non-repo should be refused")
	}
}

// Unknown host names are refused before any git/ssh work happens.
func TestDeploy_UnknownHostRefused(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "deploy", "ghost"}); code != 1 {
		t.Errorf("deploy with unknown host should exit 1, got %d", code)
	}
}
