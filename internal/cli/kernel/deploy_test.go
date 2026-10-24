package kernel

import (
	"context"
	"fmt"
	"io"
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
	calls       []string          // "<host>: <argv joined>"
	probeErr    map[string]bool   // host -> fail its phase-0 probe
	failOn      map[string]bool   // host -> fail its pull
	applyErr    map[string]bool   // host -> fail its apply
	validateErr map[string]bool   // host -> fail its auth config validate (phase 2)
	heads       map[string]string // host -> rev-parse output (default "aaa")

	// The state of a remote host's checkout, for the phase-1 sync with origin (default: level, clean).
	ahead, behind map[string]int    // host -> commits it has that origin lacks / origin has that it lacks
	dirty         map[string]string // host -> `git status --porcelain` output
	incoming      map[string]string // host -> `git diff --name-only HEAD...@{u}` output
	rebaseFail    map[string]bool   // host -> `git pull --rebase` stops on a conflict
	pushRejected  map[string]bool   // host -> `git push` is refused (origin moved)
	preflightErr  map[string]string // host -> `hemma preflight` fails with this output (uncommitted hemma files, or an old hemma)
}

func (f *fakeRunner) run(t deployTarget, argv []string) (string, error) {
	cmd := strings.Join(argv, " ")
	f.calls = append(f.calls, t.Name+": "+cmd)
	switch {
	case cmd == "true":
		if f.probeErr[t.Name] {
			return "ssh: connect to host appbox.lan port 22: Connection refused", fmt.Errorf("exit status 255")
		}
		return "", nil
	case cmd == "hemma preflight":
		if out, bad := f.preflightErr[t.Name]; bad {
			return out, fmt.Errorf("exit status 1")
		}
		return "Nothing uncommitted that hemma owns.", nil
	case strings.HasSuffix(cmd, " fetch"), strings.HasSuffix(cmd, "rebase --abort"):
		return "", nil
	case strings.Contains(cmd, "rev-list --left-right --count"):
		return fmt.Sprintf("%d\t%d", f.ahead[t.Name], f.behind[t.Name]), nil
	case strings.Contains(cmd, "status --porcelain"):
		return f.dirty[t.Name], nil
	case strings.Contains(cmd, "diff --name-only"):
		return f.incoming[t.Name], nil
	case strings.Contains(cmd, "pull --rebase"):
		if f.rebaseFail[t.Name] {
			return "CONFLICT (content): Merge conflict in core/docker-compose.yml", fmt.Errorf("exit status 1")
		}
		return "Successfully rebased.", nil
	case strings.HasSuffix(cmd, " push"):
		if f.pushRejected[t.Name] {
			return "! [rejected] master -> master (fetch first)", fmt.Errorf("exit status 1")
		}
		return "", nil
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
	case strings.Contains(cmd, "config validate"):
		if f.validateErr[t.Name] {
			return "configuration validation failed", fmt.Errorf("exit status 1")
		}
		return "Configuration parsed and loaded successfully without errors.", nil
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 0 {
		t.Fatalf("happy path should exit 0, got %d (calls: %v)", code, f.calls)
	}
	// Phase 0 probes the remote first; phase 1 brings the remote level with origin (fetch, then compare — level here, so
	// nothing more), then every host pulls: the remote at the convention path, the local at the local checkout.
	wantPrefix := []string{
		"appbox: true",
		"appbox: hemma preflight",
		"appbox: git -C ~/docker fetch",
		"appbox: git -C ~/docker rev-list --left-right --count HEAD...@{u}",
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

// Phase 0 probes only remotes (self is Local, no ssh) and happens before any
// pull.
func TestRunDeploy_Phase0ProbesRemotesOnly(t *testing.T) {
	f := &fakeRunner{}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 0 {
		t.Fatalf("happy path should exit 0, got %d (calls: %v)", code, f.calls)
	}
	// Exactly one probe, for the remote; self (resolver) is never probed.
	if got := f.count(": true"); got != 1 {
		t.Errorf("want 1 probe (remote only), got %d (calls %v)", got, f.calls)
	}
	if f.calls[0] != "appbox: true" {
		t.Errorf("probe must run first, got %q", f.calls[0])
	}
}

// Phase 0 abort: an unreachable remote stops the deploy before any pull or
// apply.
func TestRunDeploy_Phase0AbortOnUnreachable(t *testing.T) {
	f := &fakeRunner{probeErr: map[string]bool{"appbox": true}}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 1 {
		t.Fatalf("unreachable host should exit 1, got %d", code)
	}
	if f.count("pull") != 0 {
		t.Errorf("nothing must be pulled after a probe failure, got calls %v", f.calls)
	}
	if f.count("apply") != 0 {
		t.Errorf("nothing must be applied after a probe failure, got calls %v", f.calls)
	}
}

// Phase 1 abort: a pull failure anywhere stops the whole deploy — later hosts
// are not pulled and NOTHING is applied.
func TestRunDeploy_Phase1AbortOnPullFailure(t *testing.T) {
	f := &fakeRunner{failOn: map[string]bool{"appbox": true}}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 1 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 1 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", nil, nil); code != 1 {
		t.Fatalf("apply failure should exit 1, got %d", code)
	}
	if f.count("apply") != 2 {
		t.Errorf("apply must continue to remaining hosts, got calls %v", f.calls)
	}
}

// Phase 2 validates the auth config on the auth host — wherever that is — before anything is applied.
var authValidateArgv = "docker exec authelia authelia config validate"

func TestRunDeploy_AuthValidatesOnItsHostBeforeAnyApply(t *testing.T) {
	f := &fakeRunner{}
	a := &authCheck{Service: "authelia", Host: "appbox", Validate: strings.Fields(authValidateArgv)}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", a, nil); code != 0 {
		t.Fatalf("exit %d, calls %v", code, f.calls)
	}
	v := indexOfCall(f, "appbox: "+authValidateArgv)
	if v < 0 {
		t.Fatalf("auth validate must run on the auth host (a REMOTE one here), got calls %v", f.calls)
	}
	if first := indexOfCall(f, "apply"); first < v {
		t.Errorf("validate must come before any apply: %v", f.calls)
	}
	if f.count("config validate") != 1 {
		t.Errorf("validate runs once, on the auth host only: %v", f.calls)
	}
}

func TestRunDeploy_AuthValidateFailureAbortsBeforeAnyApply(t *testing.T) {
	f := &fakeRunner{validateErr: map[string]bool{"appbox": true}}
	a := &authCheck{Service: "authelia", Host: "appbox", Validate: strings.Fields(authValidateArgv)}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", a, nil); code != 1 {
		t.Fatalf("an invalid auth config must abort with 1, got %d", code)
	}
	if f.count("apply") != 0 {
		t.Errorf("nothing may be applied after a failed validate, got calls %v", f.calls)
	}
}

func TestRunDeploy_AuthHostOutsideTheDeployIsSkipped(t *testing.T) {
	f := &fakeRunner{validateErr: map[string]bool{"elsewhere": true}}
	a := &authCheck{Service: "authelia", Host: "elsewhere", Validate: strings.Fields(authValidateArgv)}
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo", a, nil); code != 0 {
		t.Fatalf("a deploy that does not touch the auth host has nothing to hold back, got %d", code)
	}
	if f.count("config validate") != 0 {
		t.Errorf("must not validate on a host outside the deploy: %v", f.calls)
	}
}

func TestPlanAuthCheck(t *testing.T) {
	cfg := &config.Config{Defaults: config.Defaults{AuthService: "authelia"}, Services: map[string]config.Service{"authelia": {Host: "edge"}}}
	a := planAuthCheck(cfg)
	if a == nil || a.Host != "edge" || strings.Join(a.Validate, " ") != authValidateArgv {
		t.Fatalf("got %+v", a)
	}
	cfg.Services["authelia"] = config.Service{Host: "edge", Disabled: true}
	if planAuthCheck(cfg) != nil {
		t.Error("a disabled auth service has nothing to validate")
	}
	if planAuthCheck(&config.Config{}) != nil {
		t.Error("no auth service: nothing to validate")
	}
}

func indexOfCall(f *fakeRunner, substr string) int {
	for i, c := range f.calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// --- preflight: deploy refuses a dirty tree, then brings the checkout level with origin (rebase + push) ---

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
	if err := deployPreflight(stdEnv(), clone, nil); err != nil {
		t.Errorf("clean pushed repo should pass preflight: %v", err)
	}

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(clone, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("services.yaml", "{}\n")
	if err := os.MkdirAll(filepath.Join(clone, "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("gen/x.conf", "1\n")
	write("hemma-manifest.yaml", "svc:\n  - gen/x.conf\n")
	git("add", "-A")
	git("commit", "-m", "hemma files")
	git("push")

	// Somebody else's file edited (not hemma's: not services.yaml, not in the manifest) → left alone, deploy goes on.
	write("f", "2")
	if err := deployPreflight(stdEnv(), clone, nil); err != nil {
		t.Errorf("an edit to a file hemma does not own must not block the deploy: %v", err)
	}
	if out := gitOut(t, clone, "status", "--porcelain"); !strings.Contains(out, " f") {
		t.Errorf("the other file's edit must be left in place, status: %q", out)
	}

	// hemma's own files edited, uncommitted → refused, naming them (and only them).
	for _, name := range []string{"services.yaml", "gen/x.conf", "hemma-manifest.yaml"} {
		old, _ := os.ReadFile(filepath.Join(clone, name))
		write(name, string(old)+"# edit\n")
		err := deployPreflight(stdEnv(), clone, nil)
		if err == nil || !strings.Contains(err.Error(), "dirty") || !strings.Contains(err.Error(), name) {
			t.Errorf("uncommitted %s should be refused, got %v", name, err)
		} else if strings.Contains(err.Error(), " f\n") || strings.HasSuffix(err.Error(), " f") {
			t.Errorf("the refusal must not list files hemma does not own: %v", err)
		}
		git("checkout", "--", name)
	}

	// A new generated file (untracked, in a directory of its own) is still seen, and refused.
	if err := os.MkdirAll(filepath.Join(clone, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("newdir/y.conf", "1\n")
	write("hemma-manifest.yaml", "svc:\n  - gen/x.conf\n  - newdir/y.conf\n")
	if err := deployPreflight(stdEnv(), clone, nil); err == nil || !strings.Contains(err.Error(), "newdir/y.conf") {
		t.Errorf("an untracked file the manifest lists should be refused, got %v", err)
	}
	git("checkout", "--", "hemma-manifest.yaml")
	os.RemoveAll(filepath.Join(clone, "newdir"))

	// Committed but unpushed → no longer refused: the deploy host pushes it itself.
	write("services.yaml", "{a: 1}\n")
	git("add", "services.yaml")
	git("commit", "-m", "local only")
	if err := deployPreflight(stdEnv(), clone, nil); err != nil {
		t.Errorf("an unpushed commit should be pushed by the preflight, got %v", err)
	}
	if out := gitOut(t, clone, "rev-list", "@{u}..HEAD"); out != "" {
		t.Errorf("after the preflight nothing may be unpushed, got %q", out)
	}

	// Not a git repo at all → refused.
	if err := deployPreflight(stdEnv(), t.TempDir(), nil); err == nil {
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

func TestSummarizePull(t *testing.T) {
	cases := []struct{ out, want string }{
		{"Already up to date.\n", "up to date"},
		{"From github.com:x/y\n   aaa..bbb  master -> origin/master\nUpdating aaa..bbb\nFast-forward\n services.yaml | 5 +\n 2 files changed, 6 insertions(+), 1 deletion(-)\n", "pulled aaa..bbb — 2 files changed, 6 insertions(+), 1 deletion(-)"},
		{"Updating ccc..ddd\nFast-forward\n", "pulled ccc..ddd"},
		{"something new git says\n", "something new git says"},
	}
	for _, c := range cases {
		if got := summarizePull(c.out); got != c.want {
			t.Errorf("summarizePull(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// gitOut runs git in dir and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes a file in dir and commits it.
func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", name)
	gitOut(t, dir, "commit", "-m", msg)
}

// otherClone is a second checkout of the same origin: another host committing to the shared repository.
func otherClone(t *testing.T, clone string) string {
	t.Helper()
	origin := gitOut(t, clone, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other")
	cmd := exec.Command("git", "clone", origin, other)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	return other
}

// The deploy host syncs with origin itself. origin having work this checkout lacks is ordinary (another host commits
// too), so deploy rebases onto it and pushes — and only a real conflict stops it, with the repo left exactly as it was.
func TestDeployPreflight_SyncsWithOrigin(t *testing.T) {
	t.Run("behind: pulled", func(t *testing.T) {
		clone := gitRepo(t)
		other := otherClone(t, clone)
		commitFile(t, other, "g", "from elsewhere", "elsewhere")
		gitOut(t, other, "push")
		var out strings.Builder
		if err := deployPreflight(&Env{Out: &out, Err: &out, NoTerminal: true}, clone, nil); err != nil {
			t.Fatalf("behind origin should be pulled, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(clone, "g")); err != nil {
			t.Error("the commit from elsewhere should be in the checkout")
		}
		if !strings.Contains(out.String(), "Pulled 1 new commit") {
			t.Errorf("should say what it did: %q", out.String())
		}
	})
	t.Run("ahead and behind, no overlap: rebased and pushed", func(t *testing.T) {
		clone := gitRepo(t)
		other := otherClone(t, clone)
		commitFile(t, other, "g", "from elsewhere", "elsewhere")
		gitOut(t, other, "push")
		commitFile(t, clone, "h", "mine", "mine") // a different file: no conflict
		var out strings.Builder
		if err := deployPreflight(&Env{Out: &out, Err: &out, NoTerminal: true}, clone, nil); err != nil {
			t.Fatalf("diverged without overlap should be rebased and pushed, got %v", err)
		}
		if log := gitOut(t, clone, "log", "--format=%s", "-3"); log != "mine\nelsewhere\ninit" {
			t.Errorf("my commit should sit on top of origin's: %q", log)
		}
		if gitOut(t, clone, "rev-list", "@{u}..HEAD") != "" || gitOut(t, clone, "rev-list", "HEAD..@{u}") != "" {
			t.Error("after the preflight the checkout must be level with origin")
		}
		if !strings.Contains(out.String(), "Rebased your 1 unpushed commit onto 1 new from origin") || !strings.Contains(out.String(), "Pushed your 1 commit") {
			t.Errorf("should say what it did: %q", out.String())
		}
	})
	t.Run("a real conflict: aborted, repo exactly as it was", func(t *testing.T) {
		clone := gitRepo(t)
		other := otherClone(t, clone)
		commitFile(t, other, "f", "theirs", "theirs") // the same file as mine
		gitOut(t, other, "push")
		commitFile(t, clone, "f", "mine", "mine")
		before := gitOut(t, clone, "rev-parse", "HEAD")
		err := deployPreflight(stdEnv(), clone, nil)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("a conflict should be refused with a plain message, got %v", err)
		}
		if after := gitOut(t, clone, "rev-parse", "HEAD"); after != before {
			t.Errorf("HEAD moved: %s -> %s", before, after)
		}
		if gitOut(t, clone, "status", "--porcelain") != "" {
			t.Error("the working tree must be clean after the abort")
		}
		if _, statErr := os.Stat(filepath.Join(clone, ".git", "rebase-merge")); statErr == nil {
			t.Error("a rebase is still in progress")
		}
		if b, _ := os.ReadFile(filepath.Join(clone, "f")); string(b) != "mine" {
			t.Errorf("my content must be intact, got %q", b)
		}
	})
	t.Run("no upstream: refused", func(t *testing.T) {
		dir := t.TempDir()
		gitOut(t, dir, "init", "-b", "main")
		commitFile(t, dir, "f", "1", "init")
		if err := deployPreflight(stdEnv(), dir, nil); err == nil || !strings.Contains(err.Error(), "upstream") {
			t.Errorf("no upstream should be refused, got %v", err)
		}
	})
}

// Phase 1 brings every REMOTE checkout level with origin before the fleet pulls: commits made there (an updater's) reach
// origin, origin's new ones reach it, edits someone has under way there are respected, and a real clash stops the deploy
// before anything is applied, naming the files.
func TestRunDeploy_Phase1SyncsRemotesWithOrigin(t *testing.T) {
	deploy := func(f *fakeRunner, resync func() error) (code int, out string) {
		var buf strings.Builder
		code = runDeploy(&Env{Out: &buf, Err: &buf, NoTerminal: true}, f, deployTestTargets, "/repo", nil, resync)
		return code, buf.String()
	}
	had := func(f *fakeRunner, call string) bool { return f.count(call) > 0 }

	t.Run("level: nothing to sync", func(t *testing.T) {
		f := &fakeRunner{}
		if code, out := deploy(f, nil); code != 0 || had(f, "pull --rebase") || had(f, " push") {
			t.Errorf("a level remote needs no sync: code=%d calls=%v\n%s", code, f.calls, out)
		}
	})
	t.Run("behind and clean: pulled with rebase", func(t *testing.T) {
		f := &fakeRunner{behind: map[string]int{"appbox": 3}}
		code, out := deploy(f, nil)
		if code != 0 || !had(f, "appbox: git -C ~/docker pull --rebase") || had(f, "--autostash") {
			t.Errorf("code=%d calls=%v", code, f.calls)
		}
		if !strings.Contains(out, "Pulled 3 new commits from origin on appbox.") {
			t.Errorf("should say what it did:\n%s", out)
		}
	})
	t.Run("commits the remote made reach origin, and this host syncs again", func(t *testing.T) {
		f := &fakeRunner{ahead: map[string]int{"appbox": 2}}
		resynced := 0
		code, out := deploy(f, func() error { resynced++; return nil })
		if code != 0 || !had(f, "appbox: git -C ~/docker push") || resynced != 1 {
			t.Errorf("an unpushed remote commit should be pushed and trigger one re-sync: code=%d resynced=%d calls=%v", code, resynced, f.calls)
		}
		if !strings.Contains(out, "Pushed appbox's 2 commits to origin.") {
			t.Errorf("should say what it did:\n%s", out)
		}
	})
	t.Run("nothing pushed: no re-sync", func(t *testing.T) {
		f := &fakeRunner{behind: map[string]int{"appbox": 1}}
		resynced := 0
		deploy(f, func() error { resynced++; return nil })
		if resynced != 0 {
			t.Errorf("a re-sync is only needed when a remote pushed, got %d", resynced)
		}
	})
	t.Run("behind with edits under way elsewhere: stashed around the rebase", func(t *testing.T) {
		f := &fakeRunner{
			behind:   map[string]int{"appbox": 1},
			dirty:    map[string]string{"appbox": " M appbox/mealie.yml\n?? notes.txt"},
			incoming: map[string]string{"appbox": "appbox/other.yml"},
		}
		if code, _ := deploy(f, nil); code != 0 || !had(f, "appbox: git -C ~/docker pull --rebase --autostash") {
			t.Errorf("disjoint edits can be carried across: code=%d calls=%v", code, f.calls)
		}
	})
	t.Run("an edit origin also changes: nothing touched, files named, nothing applied", func(t *testing.T) {
		f := &fakeRunner{
			behind:   map[string]int{"appbox": 1},
			dirty:    map[string]string{"appbox": " M appbox/docker-compose.yml"},
			incoming: map[string]string{"appbox": "appbox/docker-compose.yml\nappbox/other.yml"},
		}
		code, out := deploy(f, nil)
		if code != 1 || had(f, "pull --rebase") || had(f, "apply") || had(f, "pull --ff-only") {
			t.Fatalf("must stop before touching or applying anything: code=%d calls=%v", code, f.calls)
		}
		if !strings.Contains(out, "appbox has uncommitted edits to appbox/docker-compose.yml") || !strings.Contains(out, "nothing was touched") {
			t.Errorf("should name the files and the host:\n%s", out)
		}
	})
	t.Run("a real conflict: aborted and the deploy stops", func(t *testing.T) {
		f := &fakeRunner{behind: map[string]int{"appbox": 1}, ahead: map[string]int{"appbox": 1}, rebaseFail: map[string]bool{"appbox": true}}
		code, out := deploy(f, nil)
		if code != 1 || !had(f, "appbox: git -C ~/docker rebase --abort") || had(f, "apply") || had(f, "push") {
			t.Fatalf("a conflicting rebase must be aborted and stop the deploy: code=%d calls=%v", code, f.calls)
		}
		if !strings.Contains(out, "nothing was changed on appbox") {
			t.Errorf("should say nothing changed there:\n%s", out)
		}
	})
	t.Run("this host's re-sync fails: the deploy stops before any pull", func(t *testing.T) {
		f := &fakeRunner{ahead: map[string]int{"appbox": 1}}
		code, out := deploy(f, func() error { return fmt.Errorf("the repo has drifted after syncing with origin — deploy stopped") })
		if code != 1 || had(f, "pull --ff-only") || had(f, "apply") {
			t.Errorf("code=%d calls=%v", code, f.calls)
		}
		if !strings.Contains(out, "drifted after syncing") {
			t.Errorf("the reason should be shown:\n%s", out)
		}
	})
	t.Run("this host is never synced over ssh", func(t *testing.T) {
		f := &fakeRunner{ahead: map[string]int{"resolver": 2}}
		deploy(f, nil)
		if had(f, "resolver: git -C /repo push") || had(f, "resolver: git -C /repo fetch") {
			t.Errorf("only REMOTE hosts are reconciled in phase 1 (this one was synced up front): %v", f.calls)
		}
	})
}

// porcelainPaths must read every path whatever the status letters look like — including the first line after the
// caller trimmed its leading space (" M x" arrives as "M x").
func TestPorcelainPaths(t *testing.T) {
	for in, want := range map[string]string{
		"M a/b.yml":                       "a/b.yml",
		" M a/b.yml\n?? c d.txt":          "a/b.yml|c d.txt",
		"M  staged.yml\nMM both.yml":      "staged.yml|both.yml",
		"R  old.yml -> new.yml":           "new.yml",
		"?? \"quoted name.txt\"\n D gone": "quoted name.txt|gone",
		"":                                "",
	} {
		if got := strings.Join(porcelainPaths(in), "|"); got != want {
			t.Errorf("porcelainPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

// A remote host may have uncommitted edits (someone mid-change). Against real git: edits to files origin's new commits
// do not touch are carried across the rebase untouched; an edit to a file origin also changed — even in a different
// place — is NOT gambled on: nothing is touched and the file is named.
func TestSyncWithOrigin_DirtyRemote(t *testing.T) {
	run := func(dir string) gitFunc {
		return func(args ...string) (string, error) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
				"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
	}
	who := syncWho{owner: "core's", where: "on core"}
	say := func(string, ...any) {}

	t.Run("edits origin does not touch are carried across", func(t *testing.T) {
		clone := gitRepo(t)
		commitFile(t, clone, "g", "g1", "add g")
		gitOut(t, clone, "push")
		other := otherClone(t, clone)
		commitFile(t, other, "g", "g2", "elsewhere changes g")
		gitOut(t, other, "push")
		if err := os.WriteFile(filepath.Join(clone, "f"), []byte("my edit"), 0o644); err != nil { // f: tracked, origin did not touch it
			t.Fatal(err)
		}
		if _, err := syncWithOrigin(run(clone), who, true, nil, say); err != nil {
			t.Fatalf("disjoint edits should be carried across: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(clone, "f")); string(b) != "my edit" {
			t.Errorf("the uncommitted edit must survive, got %q", b)
		}
		if b, _ := os.ReadFile(filepath.Join(clone, "g")); string(b) != "g2" {
			t.Errorf("origin's change should have arrived, got %q", b)
		}
		if gitOut(t, clone, "stash", "list") != "" {
			t.Error("the autostash must be re-applied and dropped, not left behind")
		}
	})
	t.Run("same file, different place: refused, untouched", func(t *testing.T) {
		clone := gitRepo(t)
		if err := os.WriteFile(filepath.Join(clone, "f"), []byte("top\nmiddle\nbottom\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, clone, "add", "f")
		gitOut(t, clone, "commit", "-m", "three lines")
		gitOut(t, clone, "push")
		other := otherClone(t, clone)
		commitFile(t, other, "f", "TOP\nmiddle\nbottom\n", "elsewhere edits the top")
		gitOut(t, other, "push")
		mine := "top\nmiddle\nBOTTOM\n"
		if err := os.WriteFile(filepath.Join(clone, "f"), []byte(mine), 0o644); err != nil { // my edit is at the other end
			t.Fatal(err)
		}
		before := gitOut(t, clone, "rev-parse", "HEAD")
		_, err := syncWithOrigin(run(clone), who, true, nil, say)
		if err == nil || !strings.Contains(err.Error(), "uncommitted edits to f") || !strings.Contains(err.Error(), "nothing was touched") {
			t.Fatalf("an edit to a file origin also changed must be refused by name, got %v", err)
		}
		if gitOut(t, clone, "rev-parse", "HEAD") != before {
			t.Error("HEAD must not move")
		}
		if b, _ := os.ReadFile(filepath.Join(clone, "f")); string(b) != mine {
			t.Errorf("the edit must be intact, got %q", b)
		}
		if gitOut(t, clone, "stash", "list") != "" {
			t.Error("nothing may be stashed when the sync is refused")
		}
	})
	t.Run("unpushed commits go out even with an edit under way", func(t *testing.T) {
		clone := gitRepo(t)
		commitFile(t, clone, "h", "mine", "updater commit")
		if err := os.WriteFile(filepath.Join(clone, "f"), []byte("my edit"), 0o644); err != nil {
			t.Fatal(err)
		}
		pushed, err := syncWithOrigin(run(clone), who, true, nil, say)
		if err != nil || !pushed {
			t.Fatalf("an unpushed commit should be pushed regardless of the dirty tree: pushed=%v err=%v", pushed, err)
		}
		if gitOut(t, clone, "rev-list", "@{u}..HEAD") != "" {
			t.Error("nothing may be left unpushed")
		}
	})
}

// The ssh destination comes from services.yaml, which any host that can push to origin can write. `--` ends ssh's
// option parsing before it, so a destination starting with `-` is a (failing) host name, never an ssh option that
// runs something locally.
func TestExecRunner_SSHDestinationAfterDoubleDash(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	out, err := execRunner{env: stdEnv()}.run(deployTarget{Name: "nas", Dest: "-oProxyCommand=true"}, []string{"hemma", "apply"})
	if err != nil {
		t.Fatal(err, out)
	}
	if want := "-o\nBatchMode=yes\n--\n-oProxyCommand=true\nhemma apply\n"; out != want {
		t.Errorf("ssh argv:\n%q\nwant\n%q", out, want)
	}
}

// A failed pull is reported for what it is: only a conflict between the two sides' commits is "could not be
// combined"; anything else is a failed pull with git's own words, never "overlap with your 0 unpushed commits".
func TestSyncWithOrigin_PullFailureWording(t *testing.T) {
	fake := func(ahead, behind int, pullOut string) gitFunc {
		return func(args ...string) (string, error) {
			switch args[0] {
			case "fetch":
				return "", nil
			case "rev-list":
				return fmt.Sprintf("%d\t%d", ahead, behind), nil
			case "status", "diff":
				return "", nil
			case "pull":
				return pullOut, fmt.Errorf("exit status 1")
			case "rebase":
				return "", nil
			}
			t.Fatalf("unexpected git %v", args)
			return "", nil
		}
	}
	say := func(string, ...any) {}
	_, err := syncWithOrigin(fake(0, 1, "fatal: unable to access 'origin': Could not resolve host"), syncLocal, true, nil, say)
	if err == nil || strings.Contains(err.Error(), "overlap") || !strings.Contains(err.Error(), "pulling 1 new commit from origin failed") || !strings.Contains(err.Error(), "Could not resolve host") {
		t.Errorf("a pull that fails without a conflict must say so, with git's words: %v", err)
	}
	_, err = syncWithOrigin(fake(2, 1, "Committer identity unknown"), syncLocal, true, nil, say)
	if err == nil || strings.Contains(err.Error(), "overlap") || !strings.Contains(err.Error(), "Committer identity unknown") {
		t.Errorf("a rebase that fails for another reason is not a conflict: %v", err)
	}
	_, err = syncWithOrigin(fake(2, 1, "CONFLICT (content): Merge conflict in f\nerror: could not apply abc... mine"), syncLocal, true, nil, say)
	if err == nil || !strings.Contains(err.Error(), "1 new commit that overlap with your 2 unpushed commits") {
		t.Errorf("a real conflict keeps the conflict wording: %v", err)
	}
}

// beforePush runs after origin's commits are in and before ours go out: an error there (deploy checks for drift)
// leaves the commits unpushed, so a commit every host would refuse never reaches origin.
func TestDeployPreflight_BeforePushStopsThePush(t *testing.T) {
	clone := gitRepo(t)
	commitFile(t, clone, "h", "mine", "mine")
	calls := 0
	err := deployPreflight(stdEnv(), clone, func() error { calls++; return fmt.Errorf("the repo has drifted") })
	if err == nil || !strings.Contains(err.Error(), "the repo has drifted") || !strings.Contains(err.Error(), "stay unpushed") {
		t.Fatalf("a failing beforePush must stop the sync and say the commits were not pushed, got %v", err)
	}
	if calls != 1 {
		t.Errorf("beforePush should run once, ran %d times", calls)
	}
	if gitOut(t, clone, "rev-list", "@{u}..HEAD") == "" {
		t.Error("nothing may have been pushed")
	}
	// Level with origin: nothing to push, so the check is not even consulted.
	gitOut(t, clone, "push")
	calls = 0
	if err := deployPreflight(stdEnv(), clone, func() error { calls++; return fmt.Errorf("no") }); err != nil || calls != 0 {
		t.Errorf("with nothing to push beforePush must not run: err=%v calls=%d", err, calls)
	}
}

// The command that puts a repo back after a failed rebase runs even when the context that killed the rebase is done;
// every other command honours the context.
func TestExecRunner_RebaseAbortRunsOnADoneContext(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := execRunner{env: &Env{Out: io.Discard, Err: io.Discard, Ctx: ctx, NoTerminal: true}}
	local := deployTarget{Name: "self", Local: true}
	if out, err := r.run(local, []string{"git", "-C", "/repo", "status"}); err == nil {
		t.Errorf("an ordinary command must honour the done context, got %q", out)
	}
	out, err := r.run(local, []string{"git", "-C", "/repo", "rebase", "--abort"})
	if err != nil || !strings.Contains(out, "ran -C /repo rebase --abort") {
		t.Errorf("rebase --abort must still run: err=%v out=%q", err, out)
	}
}

// Phase 1 runs the deploy host's own dirty-tree rule on every remote (`hemma preflight` there): uncommitted hemma files
// on a remote would be applied as they are, a state origin has never seen, so the deploy stops before anything is synced
// or applied. An older hemma that does not know the command fails the same way: every host needs one that does.
func TestDeploy_RemoteWithUncommittedHemmaFilesIsRefused(t *testing.T) {
	cfg := deployCfg()
	targets, _ := resolveDeployTargets(cfg, "resolver", nil)
	f := &fakeRunner{preflightErr: map[string]string{"appbox": "The repo has uncommitted files hemma owns — commit them before deploying:\n M services.yaml"}}
	var out strings.Builder
	env := &Env{Out: &out, Err: &out, NoTerminal: true}
	if code := runDeploy(env, f, targets, "/repo", nil, nil); code != 1 {
		t.Fatalf("want exit 1, got %d\n%s", code, out.String())
	}
	for _, c := range f.calls {
		if strings.Contains(c, "pull") || strings.Contains(c, "apply") || strings.Contains(c, "fetch") {
			t.Errorf("nothing may be pulled, synced or applied after a failed preflight, got %q", c)
		}
	}
	if !strings.Contains(out.String(), "M services.yaml") || !strings.Contains(out.String(), `"appbox" is not ready to deploy`) {
		t.Errorf("the remote's own output and the host must be shown:\n%s", out.String())
	}
	// an old hemma on the remote
	f = &fakeRunner{preflightErr: map[string]string{"appbox": "hemma: unknown command \"preflight\""}}
	out.Reset()
	if code := runDeploy(env, f, targets, "/repo", nil, nil); code != 1 || !strings.Contains(out.String(), "too old") {
		t.Errorf("an old hemma must stop the deploy with the upgrade hint: %d\n%s", code, out.String())
	}
}

// `hemma preflight` is the deploy host's dirty-tree check as a command: the uncommitted hemma files, exit 1; clean, exit 0.
func TestPreflight(t *testing.T) {
	clone := gitRepo(t)
	var out strings.Builder
	env := &Env{Out: &out, Err: &out, NoTerminal: true}
	if code := cmdPreflight(env, clone, nil); code != 0 || !strings.Contains(out.String(), "Nothing uncommitted") {
		t.Errorf("clean: want 0, got %d %q", code, out.String())
	}
	if err := os.WriteFile(filepath.Join(clone, "f"), []byte("2"), 0o644); err != nil { // not hemma's
		t.Fatal(err)
	}
	out.Reset()
	if code := cmdPreflight(env, clone, nil); code != 0 {
		t.Errorf("somebody else's edit: want 0, got %d %q", code, out.String())
	}
	if err := os.WriteFile(filepath.Join(clone, "services.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := cmdPreflight(env, clone, nil); code != 1 || !strings.Contains(out.String(), "?? services.yaml") || strings.Contains(out.String(), "\n M f") {
		t.Errorf("hemma's file: want 1 naming it and only it, got %d %q", code, out.String())
	}
	if code := cmdPreflight(env, clone, []string{"x"}); code != 2 {
		t.Errorf("arguments are refused with 2, got %d", code)
	}
}
