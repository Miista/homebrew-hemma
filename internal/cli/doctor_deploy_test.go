package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// TestMain stubs doctor's known_hosts lookup for the WHOLE package: fixture
// hosts don't exist, so the real ssh-keygen -F would report every one of them
// unknown and flip doctor exit codes across unrelated tests. Individual tests
// override and restore the hook themselves.
func TestMain(m *testing.M) {
	hostKnown = func(string) bool { return true }
	os.Exit(m.Run())
}

// stubKnown swaps the known_hosts hook for one test and restores it after.
func stubKnown(t *testing.T, known func(string) bool) {
	t.Helper()
	old := hostKnown
	hostKnown = known
	t.Cleanup(func() { hostKnown = old })
}

// A host whose ip AND ssh-dest are both absent from known_hosts is a problem.
// In tests localHost matches nothing, so BOTH role-carrying hosts are remote.
func TestCheckDeployReadiness_UnknownHost(t *testing.T) {
	stubKnown(t, func(string) bool { return false })
	if got := checkDeployReadiness(deployCfg()); got != 2 {
		t.Errorf("two unknown hosts should count 2 problems, got %d", got)
	}
}

// The check passes if EITHER the ip or the ssh-dest host part is known
// (resolver is pinned known so only appbox's lookups vary).
func TestCheckDeployReadiness_KnownByEitherName(t *testing.T) {
	for _, knownName := range []string{"192.0.2.2", "appbox.lan"} {
		asked := []string{}
		stubKnown(t, func(h string) bool {
			asked = append(asked, h)
			return h == knownName || h == "resolver" || h == "192.0.2.1"
		})
		if got := checkDeployReadiness(deployCfg()); got != 0 {
			t.Errorf("host known as %q should pass, got %d problems (lookups: %v)", knownName, got, asked)
		}
	}
}

// Only remote deploy targets are checked: role-less hosts (spare) never reach
// the lookup, and an empty name is never known.
func TestCheckDeployReadiness_ChecksOnlyDeployTargets(t *testing.T) {
	asked := map[string]bool{}
	stubKnown(t, func(h string) bool { asked[h] = true; return true })
	if got := checkDeployReadiness(deployCfg()); got != 0 {
		t.Errorf("all known should pass, got %d problems", got)
	}
	if asked["192.0.2.9"] || asked["spare"] {
		t.Errorf("role-less host spare must not be checked, lookups: %v", asked)
	}
}

// The @-split for the known_hosts lookup: user@host → host, alias unchanged.
func TestSSHHostPart(t *testing.T) {
	cases := map[string]string{
		"guldmund@10.0.30.200": "10.0.30.200",
		"optiplex":             "optiplex",
		"a@b@c":                "c",
	}
	for in, want := range cases {
		if got := sshHostPart(in); got != want {
			t.Errorf("sshHostPart(%q) = %q, want %q", in, got, want)
		}
	}
}

// stubLocalHost pretends this machine is the named host, restoring after.
// localHost is a var for exactly this reason (same hook style as hostKnown).
func stubLocalHost(name string) func() {
	old := localHost
	localHost = func(*config.Config) string { return name }
	return func() { localHost = old }
}

// deploy_host scopes the readiness audit to the ONE host that fans out. On any
// other host the finding would be unfixable by design — a segmented network may
// give a non-origin host no route to its peers — and a permanently failing
// doctor trains people to ignore doctor.
func TestDeployReadiness_ScopedToDeployHost(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "app:1"}); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	stubKnown(t, func(string) bool { return false }) // nothing trusted → check fires

	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	run := func() int {
		var n int
		captureStdout(t, func() { n = checkDeployReadiness(cfg) })
		return n
	}

	restore := stubLocalHost("appbox")
	defer restore()

	// Unset: audits everywhere — the pre-existing behaviour, so adoption is opt-in.
	if run() == 0 {
		t.Error("with deploy_host unset the check must run on any host")
	}
	// Set to the OTHER host: silent here.
	cfg.Defaults.DeployHost = "resolver"
	if n := run(); n != 0 {
		t.Errorf("a non-origin host must report no deploy problems, got %d", n)
	}
	// Set to THIS host: audited again.
	cfg.Defaults.DeployHost = "appbox"
	if run() == 0 {
		t.Error("the deploy_host itself must still be audited")
	}
	// A machine matching no declared host is not the origin either.
	restore()
	restore = stubLocalHost("")
	if n := run(); n != 0 {
		t.Errorf("an unidentified machine must not audit, got %d", n)
	}
}

// deploy REFUSES to run anywhere but the deploy_host: a partial fan-out that
// dies host-by-host on connection timeouts leaves the fleet in a mixed state,
// so it declines before the preflight and before any ssh.
func TestDeploy_RefusedFromNonDeployHost(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "app:1"}); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	if code := Run([]string{"-C", dir, "set", "deploy-host", "resolver"}); code != 0 {
		t.Fatalf("set deploy-host exit %d", code)
	}

	// Assert on the MESSAGE, not just the exit code: deployPreflight also fails
	// in a fixture repo (not a clean, pushed checkout), so exit==1 alone would
	// pass with the guard removed — verified by stubbing the guard out.
	for _, self := range []string{"appbox", ""} {
		restore := stubLocalHost(self)
		var code int
		errOut := captureStderr(t, func() { code = Run([]string{"-C", dir, "deploy"}) })
		restore()
		if code != 1 {
			t.Errorf("deploy from self=%q should exit 1, got %d", self, code)
		}
		if !strings.Contains(errOut, `deploy_host is "resolver"`) ||
			!strings.Contains(errOut, "refusing to deploy from") {
			t.Errorf("self=%q: expected the deploy_host refusal, got:\n%s", self, errOut)
		}
		// It must decline BEFORE the preflight, so that error must not appear.
		if strings.Contains(errOut, "must already carry the change") {
			t.Errorf("self=%q: refusal must precede the preflight, got:\n%s", self, errOut)
		}
	}
}

// An unknown host name is refused before it can be persisted (mirroring
// `set dns-host`), and '-' clears the role without leaving the key behind.
func TestSetDeployHost_ValidationAndClear(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)

	if code := Run([]string{"-C", dir, "set", "deploy-host", "nope"}); code == 0 {
		t.Error("an unknown host must be refused")
	}
	load := func() *config.Config {
		c, err := config.Load(filepath.Join(dir, "services.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := load().Defaults.DeployHost; got != "" {
		t.Errorf("a refused name must not persist, got %q", got)
	}

	if code := Run([]string{"-C", dir, "set", "deploy-host", "resolver"}); code != 0 {
		t.Fatalf("set exit %d", code)
	}
	if got := load().Defaults.DeployHost; got != "resolver" {
		t.Errorf("deploy_host = %q, want resolver", got)
	}

	if code := Run([]string{"-C", dir, "set", "deploy-host", "-"}); code != 0 {
		t.Fatalf("clear exit %d", code)
	}
	if got := load().Defaults.DeployHost; got != "" {
		t.Errorf("'-' must clear the role, got %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "services.yaml"))
	if strings.Contains(string(b), "deploy_host") {
		t.Errorf("a cleared role must not be re-emitted:\n%s", b)
	}
}
