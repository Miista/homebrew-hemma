package cli

// Snapshot (characterization) tests: every CLI command is run in-process
// against a copy of an anonymized real-world services.yaml and its observable
// behaviour — exit code, stdout, stderr, and which files it created, changed
// or removed — is compared with a golden file under testdata/snapshot/golden.
//
// They exist to prove that refactoring the CLI internals (the kernel
// extraction) changes nothing a user or script can see. Generated file
// CONTENTS are deliberately not stored (internal/plan and internal/render own
// those); each is recorded as a short hash, so any change still shows up.
//
// Regenerate after an intended behaviour change:
//
//	go test ./internal/cli/ -run TestSnapshots -update
//
// The fixture is synthetic (RFC 5737 addresses, example domain). Never copy a
// real services.yaml into testdata.

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"hemma/internal/cli/kernel"
	"hemma/internal/config"
)

var updateSnapshots = flag.Bool("update", false, "rewrite the snapshot golden files")

const snapshotDir = "testdata/snapshot"

// snapCase is one command invocation. setup commands run first (silently, same
// repo); args is what is snapshotted.
type snapCase struct {
	name  string
	args  string   // space-separated; '' is an empty argument
	setup []string // each a space-separated command (or @-directive) run before args
	role  string   // pretend this machine is that host (default: matches none)
	tools string   // "" none on PATH; "ok" fake tools succeed; "fail" they fail
}

// withTools returns c running as host `role` with fake external tools.
func (c snapCase) withTools(role, tools string) snapCase {
	c.role, c.tools = role, tools
	return c
}

// fakeTools are the external programs hemma shells out to. Each fake records
// its argv to $FAKE_LOG and exits 0 (or 1 with a message when FAKE_MODE=fail),
// so the golden file pins the exact external command sequence.
var fakeTools = []string{"docker", "caddy", "dig", "ssh", "ssh-keygen", "git", "curl", "bash"}

const fakeToolScript = `#!/bin/sh
echo "${0##*/} $*" >> "$FAKE_LOG"
if [ "$FAKE_MODE" = fail ]; then
	echo "fake ${0##*/}: simulated failure" >&2
	exit 1
fi
exit 0
`

// sc builds a case from a name and a command line.
func sc(name, args string, setup ...string) snapCase {
	return snapCase{name: name, args: args, setup: setup}
}

// synced is the setup that brings generated files in line with services.yaml.
const synced = "doctor --fix"

// secretish masks the values that differ on every run (minted credentials).
var secretish = []*regexp.Regexp{
	regexp.MustCompile(`(Client ID: )\S+`),
	regexp.MustCompile(`(Client Secret \([^)]*\): )\S+`),
	regexp.MustCompile(`(client_id: ')[^']+`),
	regexp.MustCompile(`(client_secret: ')[^']+`),
}

func snapshotCases() []snapCase {
	var cs []snapCase
	add := func(c ...snapCase) { cs = append(cs, c...) }

	// ── top level ────────────────────────────────────────────────────────────
	add(
		sc("top-no-args", ""),
		sc("top-chdir-missing-arg", "-C"),
		sc("top-chdir-only", "-C ."),
		sc("top-unknown-command", "frobnicate"),
		sc("top-version", "version"),
		sc("top-version-dash-dash", "--version"),
		sc("top-version-dash-v", "-v"),
		sc("top-help", "help"),
		sc("top-help-flag", "--help"),
		sc("top-help-short", "-h"),
	)

	// ── per-command help: `help <cmd>` and `<cmd> --help` ────────────────────
	for _, c := range []string{
		"add", "update", "remove", "enable", "disable", "rename", "defaults",
		"create", "list", "verify", "doctor", "apply", "deploy", "measure",
		"completion", "version", "help", "nonsense",
		"add service", "add host", "add domain", "update service", "update host",
		"remove service", "remove host", "remove domain", "create app", "create user",
		"defaults set",
	} {
		slug := strings.ReplaceAll(c, " ", "-")
		add(
			sc("help-"+slug, "help "+c),
			sc("help-flag-"+slug, c+" --help"),
		)
	}

	// ── completion ───────────────────────────────────────────────────────────
	add(
		sc("completion-bash", "completion bash"),
		sc("completion-zsh", "completion zsh"),
		sc("completion-fish-unsupported", "completion fish"),
		sc("completion-missing-shell", "completion"),
	)

	// ── list ─────────────────────────────────────────────────────────────────
	for _, st := range []struct{ tag, setup string }{{"fresh", ""}, {"synced", synced}} {
		var setup []string
		if st.setup != "" {
			setup = []string{st.setup}
		}
		add(
			sc("list-"+st.tag, "list", setup...),
			sc("list-all-"+st.tag, "list --all", setup...),
			sc("list-policy-"+st.tag, "list --policy", setup...),
			sc("list-all-policy-"+st.tag, "list --all --policy", setup...),
			sc("list-json-"+st.tag, "list --json", setup...),
		)
	}
	add(
		sc("list-bad-flag", "list --bogus"),
		sc("list-extra-arg", "list nonsense"),
	)

	// ── defaults ─────────────────────────────────────────────────────────────
	add(
		sc("defaults-show", "defaults"),
		sc("defaults-unknown-sub", "defaults frob"),
		sc("defaults-set-missing", "defaults set"),
		sc("defaults-set-unknown-key", "defaults set colour blue"),
		sc("defaults-set-dns-host", "defaults set dns-host edge", synced),
		sc("defaults-set-dns-host-unknown-host", "defaults set dns-host nowhere"),
		sc("defaults-set-dns-host-missing-value", "defaults set dns-host"),
		sc("defaults-set-dns-host-clear", "defaults set dns-host -"),
		sc("defaults-set-deploy-host", "defaults set deploy-host edge", synced),
		sc("defaults-set-deploy-host-clear", "defaults set deploy-host -"),
		sc("defaults-set-deploy-host-unknown", "defaults set deploy-host nowhere"),
		sc("defaults-set-auth-snippet", "defaults set auth-snippet shared/other.caddy", synced),
		sc("defaults-set-auth-snippet-clear", "defaults set auth-snippet -", synced),
		sc("defaults-set-auth-service", "defaults set auth-service status", synced),
		sc("defaults-set-auth-service-clear", "defaults set auth-service -", synced),
		sc("defaults-set-auth-service-unknown", "defaults set auth-service nowhere"),
		sc("defaults-set-local-networks", "defaults set local-networks 198.51.100.0/24,203.0.113.0/24", synced),
		sc("defaults-set-local-networks-clear", "defaults set local-networks -", synced),
		sc("defaults-set-local-networks-invalid", "defaults set local-networks not-a-cidr"),
		sc("defaults-set-tunnel-dir", "defaults set tunnel-dir tunnels", synced),
		sc("defaults-set-tunnel-dir-clear", "defaults set tunnel-dir -", synced),
	)

	// ── services ─────────────────────────────────────────────────────────────
	add(
		sc("add-service-minimal", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080", synced),
		sc("add-service-minimal-fresh", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080"),
		sc("add-service-short-flags", "add service newsvc -f newsvc.home.example --host core --backend newsvc:8080", synced),
		sc("add-service-forward-groups", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --auth-mode forward --auth-groups admins,readers", synced),
		sc("add-service-oidc", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --auth-mode oidc", synced),
		sc("add-service-external", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --auth-mode external", synced),
		sc("add-service-tunnel", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --tunnel", synced),
		sc("add-service-public-alias", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --public", synced),
		sc("add-service-bypass-paths", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --auth-mode forward --bypass-paths /healthz,/ping", synced),
		// Mode none + bypass paths/policy is saved and then skipped by the plan (reported, exit 1). The file hemma
		// wrote must still be loadable: it once wrote `mode: ""`, which nothing could read back.
		sc("add-service-none-with-bypass-paths", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --bypass-paths /api", synced),
		sc("list-after-add-service-none-with-bypass-paths", "list --all", synced, "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --bypass-paths /api"),
		sc("doctor-after-add-service-none-with-bypass-paths", "doctor", synced, "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --bypass-paths /api"),
		sc("update-service-add-bypass-paths-to-unprotected", "update service dash --auth-mode none --bypass-paths /api", synced),
		sc("add-service-duplicate", "add service dash --fqdn dash2.home.example --host edge --backend dash:3000"),
		sc("add-service-missing-name", "add service"),
		sc("add-service-no-flags-non-tty", "add service newsvc"),
		sc("add-service-missing-fqdn", "add service newsvc --host edge --backend newsvc:8080"),
		sc("add-service-missing-host", "add service newsvc --fqdn newsvc.home.example --backend newsvc:8080"),
		sc("add-service-missing-backend", "add service newsvc --fqdn newsvc.home.example --host edge"),
		sc("add-service-unknown-host", "add service newsvc --fqdn newsvc.home.example --host nowhere --backend newsvc:8080"),
		sc("add-service-fqdn-no-domain", "add service newsvc --fqdn newsvc.other.example --host edge --backend newsvc:8080"),
		sc("add-service-bad-auth-mode", "add service newsvc --fqdn newsvc.home.example --host edge --backend newsvc:8080 --auth-mode magic"),
		sc("add-service-bad-flag", "add service newsvc --bogus"),

		sc("update-service-backend", "update service dash --backend dash:4000", synced),
		sc("update-service-fqdn", "update service dash --fqdn dashboard.home.example", synced),
		sc("update-service-host", "update service dash --host core", synced),
		sc("update-service-auth-none", "update service dash --auth-mode none", synced),
		sc("update-service-auth-none-with-groups", "update service robot --auth-mode none", synced),
		sc("update-service-auth-forward", "update service dash --auth-mode forward --auth-groups admins", synced),
		sc("update-service-auth-groups", "update service robot --auth-groups ops,admins", synced),
		sc("update-service-all-fields", "update service dash --fqdn d.home.example --host core --backend d:1 --auth-mode external", synced),
		sc("update-service-fresh", "update service dash --backend dash:4000"),
		sc("update-service-unknown", "update service nowhere --backend x:1"),
		sc("update-service-missing-name", "update service"),
		sc("update-service-no-flags-non-tty", "update service dash"),
		sc("update-service-unknown-host", "update service dash --host nowhere"),
		sc("update-service-fqdn-no-domain", "update service dash --fqdn dash.other.example"),
		sc("update-service-bad-auth-mode", "update service dash --auth-mode magic"),
		sc("update-service-bad-flag", "update service dash --bogus"),

		sc("remove-service", "remove service dash", synced),
		sc("remove-service-fresh", "remove service dash"),
		sc("remove-service-disabled", "remove service shelf", synced),
		sc("remove-service-auth-service", "remove service sso", synced),
		sc("remove-service-unknown", "remove service nowhere"),
		sc("remove-service-missing-name", "remove service"),

		sc("disable-service", "disable service dash", synced),
		sc("disable-service-already", "disable service shelf", synced),
		sc("disable-service-unknown", "disable service nowhere"),
		sc("disable-service-missing-name", "disable service"),
		sc("enable-service", "enable service shelf", synced),
		sc("enable-service-already", "enable service dash", synced),
		sc("enable-service-unknown", "enable service nowhere"),
		sc("enable-service-missing-name", "enable service"),

		sc("rename-service", "rename service dash dashboard", synced),
		sc("rename-service-fresh", "rename service dash dashboard"),
		sc("rename-service-disabled", "rename service shelf bookshelf", synced),
		sc("rename-service-to-existing", "rename service dash wiki", synced),
		sc("rename-service-auth-service", "rename service sso login", synced),
		sc("rename-service-unknown", "rename service nowhere x"),
		sc("rename-service-same-name", "rename service dash dash"),
		sc("rename-service-missing-args", "rename service dash"),
		sc("rename-service-no-args", "rename service"),
	)

	// ── hosts ────────────────────────────────────────────────────────────────
	add(
		sc("add-host", "add host lab 192.0.2.30", "@mkdir lab", synced),
		sc("add-host-no-directory", "add host lab 192.0.2.30"),
		sc("add-host-ssh-proxies", "add host lab 192.0.2.30 --ssh ops@192.0.2.30 --trusted-proxies 198.51.100.11/32,198.51.100.12", "@mkdir lab", synced),
		sc("add-host-duplicate", "add host edge 192.0.2.99"),
		sc("add-host-bad-ip", "add host lab not-an-ip"),
		sc("add-host-missing-ip", "add host lab"),
		sc("add-host-missing-name", "add host"),
		sc("add-host-bad-proxy", "add host lab 192.0.2.30 --trusted-proxies nonsense"),
		sc("update-host-ip", "update host edge --ip 192.0.2.21", synced),
		sc("update-host-ssh", "update host edge --ssh deploy@192.0.2.20", synced),
		sc("update-host-proxies", "update host edge --trusted-proxies 198.51.100.77/32", synced),
		sc("update-host-clear-proxies", "update host edge --trusted-proxies ''", synced),
		sc("update-host-unknown", "update host nowhere --ip 192.0.2.50"),
		sc("update-host-bad-ip", "update host edge --ip nope"),
		sc("update-host-no-flags-non-tty", "update host edge"),
		sc("update-host-missing-name", "update host"),
	)
	// remove host: an unused host needs a prior add; an in-use host is refused.
	add(
		sc("remove-host-unused", "remove host lab", "@mkdir lab", "add host lab 192.0.2.30", synced),
		sc("remove-host-in-use", "remove host edge", synced),
		sc("remove-host-dns-host", "remove host core", synced),
		sc("remove-host-unknown", "remove host nowhere"),
		sc("remove-host-missing-name", "remove host"),
		sc("host-unsupported-verb", "enable host edge"),
		sc("domain-unsupported-verb", "update domain home.example"),
	)

	// ── domains ──────────────────────────────────────────────────────────────
	add(
		sc("add-domain", "add domain lab.example", synced),
		sc("add-domain-duplicate", "add domain home.example"),
		sc("add-domain-missing-name", "add domain"),
		sc("remove-domain-in-use", "remove domain home.example", synced),
		sc("remove-domain-unused", "remove domain lab.example", "add domain lab.example", synced),
		sc("remove-domain-unknown", "remove domain nowhere.example"),
		sc("remove-domain-missing-name", "remove domain"),
	)

	// ── verb/noun routing errors ─────────────────────────────────────────────
	add(
		sc("add-no-noun", "add"),
		sc("update-no-noun", "update"),
		sc("remove-no-noun", "remove"),
		sc("enable-no-noun", "enable"),
		sc("disable-no-noun", "disable"),
		sc("rename-no-noun", "rename"),
		sc("add-unknown-noun", "add widget x"),
	)

	// ── create (credentials) ─────────────────────────────────────────────────
	add(
		sc("create-app-oidc", "create app oidc foo"),
		sc("create-app-oidc-callback", "create app oidc foo /oauth/callback"),
		sc("create-app-missing-name", "create app oidc"),
		sc("create-app-unknown-kind", "create app saml foo"),
		sc("create-user-non-tty", "create user bob"),
		sc("create-user-missing-name", "create user"),
		sc("create-no-noun", "create"),
		sc("create-unknown-noun", "create widget x"),
	)

	// ── apply / verify / doctor / deploy / measure ───────────────────────────
	// These shell out (docker, caddy, dig, ssh, git, bash). The harness runs
	// with an empty PATH, so what is pinned here is every decision hemma makes
	// BEFORE or WITHOUT reaching an external tool.
	add(
		sc("apply-fresh-drift", "apply"),
		sc("apply-synced-no-role", "apply", synced),
		sc("apply-bad-flag", "apply --bogus"),
		sc("verify-no-role", "verify"),
		sc("verify-all", "verify --all"),
		sc("verify-fqdn", "verify dash.home.example"),
		sc("verify-all-fqdn", "verify --all dash.home.example"),
		sc("verify-unknown-fqdn", "verify nowhere.example"),
		sc("verify-bad-flag", "verify --bogus"),
		sc("doctor-fresh", "doctor"),
		sc("doctor-synced", "doctor", synced),
		sc("doctor-fix-fresh", "doctor --fix"),
		sc("doctor-fix-synced-idempotent", "doctor --fix", synced),
		sc("doctor-bad-flag", "doctor --bogus"),
		sc("deploy-wrong-host", "deploy"),
		sc("deploy-explicit-host", "deploy edge"),
		sc("deploy-unknown-host", "deploy nowhere"),
		sc("measure-service", "measure dash"),
		sc("measure-fqdn", "measure dash.home.example"),
		sc("measure-url", "measure https://example.com/"),
		sc("measure-compare", "measure --compare dash"),
		sc("measure-runs", "measure -n 3 -w 1 dash"),
		sc("measure-missing-target", "measure"),
		sc("measure-unknown-service", "measure nowhere"),
		sc("measure-bad-flag", "measure --bogus"),
	)

	// Past the host-match gate: this machine is "core" (dns-host, caddy, auth
	// provider) or "edge" (caddy only), with fake docker/caddy/dig/ssh/git/…
	// that either all succeed or all fail.
	for _, role := range []string{"core", "edge"} {
		for _, mode := range []string{"ok", "fail"} {
			tag := "-as-" + role + "-" + mode
			add(
				sc("apply"+tag, "apply", synced).withTools(role, mode),
				sc("apply-fresh"+tag, "apply").withTools(role, mode),
				sc("verify"+tag, "verify", synced).withTools(role, mode),
				sc("verify-all"+tag, "verify --all", synced).withTools(role, mode),
				sc("verify-fqdn"+tag, "verify dash.home.example", synced).withTools(role, mode),
				sc("verify-unknown-fqdn"+tag, "verify nowhere.example", synced).withTools(role, mode),
				sc("doctor"+tag, "doctor", synced).withTools(role, mode),
				sc("doctor-fresh"+tag, "doctor").withTools(role, mode),
				sc("doctor-fix"+tag, "doctor --fix").withTools(role, mode),
				sc("deploy"+tag, "deploy", synced).withTools(role, mode),
				sc("deploy-one-host"+tag, "deploy edge", synced).withTools(role, mode),
				sc("deploy-unknown-host"+tag, "deploy nowhere", synced).withTools(role, mode),
				sc("measure"+tag, "measure dash", synced).withTools(role, mode),
				sc("measure-compare"+tag, "measure --compare dash", synced).withTools(role, mode),
				sc("measure-url"+tag, "measure -n 2 -w 0 https://example.com/", synced).withTools(role, mode),
			)
		}
	}
	// A repo with no deploy_host restriction deploys from anywhere.
	add(
		sc("deploy-unrestricted-ok", "deploy", "defaults set deploy-host -", synced).withTools("", "ok"),
		sc("deploy-unrestricted-fail", "deploy", "defaults set deploy-host -", synced).withTools("", "fail"),
	)

	// ── malformed repos ──────────────────────────────────────────────────────
	add(
		snapCase{name: "repo-missing-config", args: "list", setup: []string{"@rm services.yaml"}},
		snapCase{name: "repo-unparseable-config", args: "list", setup: []string{"@write services.yaml hosts: [unclosed"}},
		snapCase{name: "repo-add-to-unparseable", args: "add domain x.example", setup: []string{"@write services.yaml hosts: [unclosed"}},
		snapCase{name: "repo-sync-with-orphan-manifest", args: "doctor --fix", setup: []string{synced, "@rm hemma-manifest.yaml"}},
	)
	return cs
}

// runCLI invokes Run in-process with stdout/stderr/stdin captured, returning
// the exit code and both streams.
func runCLI(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	outF, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	oo, oe, oi := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = outF, errF, devnull
	func() {
		defer func() { os.Stdout, os.Stderr, os.Stdin = oo, oe, oi }()
		code = Run(args)
	}()
	outF.Close()
	errF.Close()
	devnull.Close()
	ob, _ := os.ReadFile(outF.Name())
	eb, _ := os.ReadFile(errF.Name())
	return code, string(ob), string(eb)
}

// fingerprint hashes every file under root (slash paths → short sha256).
func fingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		sum := sha256.Sum256(b)
		m[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:6])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// materialize copies the anonymized fixture into dir and creates the host
// directories ("a host's name is its repo directory, which must exist").
func materialize(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join(snapshotDir, "repo")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"core", "edge"} {
		if err := os.MkdirAll(filepath.Join(dir, host), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// applySetup runs one setup step: a plain command line, or an "@"-directive
// that tampers with the repo (@rm <path>, @write <path> <content>).
func applySetup(t *testing.T, dir, step string) {
	t.Helper()
	switch {
	case strings.HasPrefix(step, "@mkdir "):
		if err := os.MkdirAll(filepath.Join(dir, strings.TrimPrefix(step, "@mkdir ")), 0o755); err != nil {
			t.Fatal(err)
		}
	case strings.HasPrefix(step, "@rm "):
		os.Remove(filepath.Join(dir, strings.TrimPrefix(step, "@rm ")))
	case strings.HasPrefix(step, "@write "):
		parts := strings.SplitN(strings.TrimPrefix(step, "@write "), " ", 2)
		if err := os.WriteFile(filepath.Join(dir, parts[0]), []byte(parts[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	default:
		runCLI(t, append([]string{"-C", dir}, strings.Fields(step)...))
	}
}

// tokens splits a command line on spaces, treating ” as an empty argument
// (needed for `--trusted-proxies ”`).
func tokens(line string) []string {
	var out []string
	for _, f := range strings.Fields(line) {
		if f == "''" {
			f = ""
		}
		out = append(out, f)
	}
	return out
}

func TestSnapshots(t *testing.T) {
	// A hermetic environment: no colour, no external tools on PATH (so
	// docker/caddy/dig/ssh/git/bash are all "not installed"), and a HOME that
	// is not the developer's.
	t.Setenv("NO_COLOR", "1")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	seen := map[string]bool{}
	for _, c := range snapshotCases() {
		if seen[c.name] {
			t.Fatalf("duplicate snapshot case name %q", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			materialize(t, dir)
			for _, s := range c.setup {
				applySetup(t, dir, s)
			}
			before := fingerprint(t, dir)

			// Environment for this case: PATH holds only the fake tools (or
			// nothing), and the machine optionally "is" one of the hosts.
			bin := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "calls.log")
			if c.tools != "" {
				for _, name := range fakeTools {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeToolScript), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Setenv("PATH", bin)
			t.Setenv("FAKE_LOG", logPath)
			t.Setenv("FAKE_MODE", c.tools)
			if c.role != "" {
				role := c.role
				defer kernel.SetLocalHost(func(*config.Config) string { return role })()
			}

			argv := tokens(c.args)
			full := argv
			// Cases that exercise -C themselves pass it explicitly.
			if len(argv) == 0 || (argv[0] != "-C" && argv[0] != "--chdir") {
				full = append([]string{"-C", dir}, argv...)
			}
			if c.args == "" {
				full = nil // bare `hemma`
			}
			code, stdout, stderr := runCLI(t, full)
			after := fingerprint(t, dir)

			calls, _ := os.ReadFile(logPath)
			got := snapshotText(c, dir, code, stdout, stderr, before, after, string(calls))
			golden := filepath.Join(snapshotDir, "golden", c.name+".txt")
			if *updateSnapshots {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("no golden file %s — run with -update to create it", golden)
			}
			if string(want) != got {
				t.Errorf("snapshot mismatch for %q\n--- want (%s)\n%s\n--- got\n%s", c.name, golden, want, got)
			}
		})
	}

	// Golden files with no case are leftovers from a renamed/removed case.
	if !*updateSnapshots {
		entries, _ := os.ReadDir(filepath.Join(snapshotDir, "golden"))
		for _, e := range entries {
			if !seen[strings.TrimSuffix(e.Name(), ".txt")] {
				t.Errorf("orphaned golden file %s (no such case)", e.Name())
			}
		}
	}
}

// snapshotText formats one run as the golden-file text.
func snapshotText(c snapCase, dir string, code int, stdout, stderr string, before, after map[string]string, calls string) string {
	norm := func(s string) string {
		for _, d := range []string{dir, evalSymlinks(dir)} {
			s = strings.ReplaceAll(s, d, "<repo>")
		}
		for _, re := range secretish {
			s = re.ReplaceAllString(s, "${1}<random>")
		}
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "$ hemma %s\n", c.args)
	if len(c.setup) > 0 {
		fmt.Fprintf(&b, "# setup: %s\n", strings.Join(c.setup, " ; "))
	}
	if c.role != "" || c.tools != "" {
		fmt.Fprintf(&b, "# machine: host=%q tools=%q\n", c.role, c.tools)
	}
	fmt.Fprintf(&b, "exit: %d\n", code)
	fmt.Fprintf(&b, "--- stdout ---\n%s", ensureNL(norm(stdout)))
	fmt.Fprintf(&b, "--- stderr ---\n%s", ensureNL(norm(stderr)))

	var changes []string
	for p, h := range after {
		switch old, ok := before[p]; {
		case !ok:
			changes = append(changes, fmt.Sprintf("+ %s %s", p, h))
		case old != h:
			changes = append(changes, fmt.Sprintf("~ %s %s", p, h))
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			changes = append(changes, fmt.Sprintf("- %s", p))
		}
	}
	sort.Strings(changes)
	if c.tools != "" {
		fmt.Fprintf(&b, "--- external commands ---\n%s", ensureNL(norm(calls)))
	}
	fmt.Fprintf(&b, "--- files ---\n%s", ensureNL(strings.Join(changes, "\n")))
	// The config is small and is what mutations are about: keep it readable.
	if after["services.yaml"] != before["services.yaml"] {
		if raw, err := os.ReadFile(filepath.Join(dir, "services.yaml")); err == nil {
			fmt.Fprintf(&b, "--- services.yaml ---\n%s", ensureNL(norm(string(raw))))
		}
	}
	return b.String()
}

func evalSymlinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func ensureNL(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
