package kernel

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
	probeErr map[string]bool   // host -> fail its phase-0 probe
	failOn   map[string]bool   // host -> fail its pull
	applyErr map[string]bool   // host -> fail its apply
	heads    map[string]string // host -> rev-parse output (default "aaa")
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 0 {
		t.Fatalf("happy path should exit 0, got %d (calls: %v)", code, f.calls)
	}
	// Phase 0 probes the remote first, then remote pulls the convention path
	// and local pulls the local checkout.
	wantPrefix := []string{
		"appbox: true",
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 0 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 1 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 1 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 1 {
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
	if code := runDeploy(stdEnv(), f, deployTestTargets, "/repo"); code != 1 {
		t.Fatalf("apply failure should exit 1, got %d", code)
	}
	if f.count("apply") != 2 {
		t.Errorf("apply must continue to remaining hosts, got calls %v", f.calls)
	}
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
	if err := deployPreflight(stdEnv(), clone); err != nil {
		t.Errorf("clean pushed repo should pass preflight: %v", err)
	}

	// Dirty working tree → refused.
	if err := os.WriteFile(filepath.Join(clone, "f"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deployPreflight(stdEnv(), clone); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Errorf("dirty repo should be refused, got %v", err)
	}

	// Committed but unpushed → no longer refused: the deploy host pushes it itself.
	git("add", "f")
	git("commit", "-m", "local only")
	if err := deployPreflight(stdEnv(), clone); err != nil {
		t.Errorf("an unpushed commit should be pushed by the preflight, got %v", err)
	}
	if out := gitOut(t, clone, "rev-list", "@{u}..HEAD"); out != "" {
		t.Errorf("after the preflight nothing may be unpushed, got %q", out)
	}

	// Not a git repo at all → refused.
	if err := deployPreflight(stdEnv(), t.TempDir()); err == nil {
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
		if err := deployPreflight(&Env{Out: &out, Err: &out, NoTerminal: true}, clone); err != nil {
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
		if err := deployPreflight(&Env{Out: &out, Err: &out, NoTerminal: true}, clone); err != nil {
			t.Fatalf("diverged without overlap should be rebased and pushed, got %v", err)
		}
		if log := gitOut(t, clone, "log", "--format=%s", "-3"); log != "mine\nelsewhere\ninit" {
			t.Errorf("my commit should sit on top of origin's: %q", log)
		}
		if gitOut(t, clone, "rev-list", "@{u}..HEAD") != "" || gitOut(t, clone, "rev-list", "HEAD..@{u}") != "" {
			t.Error("after the preflight the checkout must be level with origin")
		}
		if !strings.Contains(out.String(), "Rebased your 1 unpushed commit onto 1 new from origin") || !strings.Contains(out.String(), "Pushed 1 commit") {
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
		err := deployPreflight(stdEnv(), clone)
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
		if err := deployPreflight(stdEnv(), dir); err == nil || !strings.Contains(err.Error(), "upstream") {
			t.Errorf("no upstream should be refused, got %v", err)
		}
	})
}
