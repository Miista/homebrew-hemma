package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"hemma/internal/config"
)

// cmdDeploy is the push-based fan-out: make the whole fleet match origin.
//
//	hemma deploy [<host> ...]
//
// deploy is the ONE deliberate exception to the "no SSH" non-goal (design
// §12) — the precedent being `apply`, which relaxed the "no reloads" non-goal
// the same way. It runs exactly two things per host, in two strictly ordered
// phases:
//
//	Phase 1 — pull everywhere:  git -C ~/docker pull --ff-only
//	Phase 2 — apply everywhere: hemma apply   (remotes first, self LAST)
//
// Phase 1 is all-or-nothing: `pull --ff-only` either fast-forwards or refuses
// touching nothing, so ANY failure (divergence, dirty remote tree, unreachable
// host, ssh auth) aborts the entire deploy before phase 2 starts — no runtime
// state has changed anywhere. Pulled-but-not-applied checkouts are the normal
// intermediate state of the manual workflow, so an abort leaves nothing to
// undo. After the pulls, every host must be on the SAME commit (a push racing
// between pulls would silently deploy different states) — a mismatch also
// aborts.
//
// Phase 2 is per-host best-effort: each host's `apply` is internally
// consistent (it validates before restarting anything), so a failure on one
// host is reported and the fan-out CONTINUES — stopping mid-fan-out would just
// leave a less converged fleet. Remotes go first and self LAST: applying on
// self restarts the resolver, and a resolver bounce mid-fan-out would break
// DNS-resolved ssh to the remaining hosts (converge the leaves, bounce the
// resolver once at the end).
//
// Targets default to every host with a role (runs a non-disabled service, or
// is the dns_host); explicit names restrict the set. A host is reached via
// `ssh -o BatchMode=yes <dest>` where <dest> is its verbatim `ssh:` field
// (default: the host name); a target whose IP matches this machine runs its
// commands locally instead (same localHost rule as apply). BatchMode forbids
// interactive prompts — auth failures abort in phase 1, where they're
// harmless.
//
// Deploying means "make the fleet match origin", so origin must already carry
// the intent: deploy refuses up front if the LOCAL repo is dirty or has
// unpushed commits.
func cmdDeploy(repoRoot, cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "deploy")
	if cfg == nil {
		return code
	}
	self := localHost(cfg)
	targets, err := resolveDeployTargets(cfg, self, args)
	if err != nil {
		errf("%v", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Println("Nothing to deploy: no host runs a service or is the dns_host.")
		return 0
	}
	if err := deployPreflight(repoRoot); err != nil {
		errf("%v", err)
		hint("Deploy means \"make the fleet match origin\" — origin must already carry the change.")
		return 1
	}
	return runDeploy(execRunner{}, targets, repoRoot)
}

// deployTarget is one host the deploy fans out to.
type deployTarget struct {
	Name  string
	Dest  string // verbatim ssh(1) destination; unused when Local
	Local bool   // this machine — run commands locally, no ssh-to-self
}

// deployRunner runs one command on one target. It exists so the phase logic
// is testable with a fake runner — real ssh is deliberately untested.
type deployRunner interface {
	run(t deployTarget, argv []string) (output string, err error)
}

// execRunner is the real runner: local exec, or ssh with BatchMode (no
// interactive prompts from inside a fan-out).
type execRunner struct{}

func (execRunner) run(t deployTarget, argv []string) (string, error) {
	if !t.Local {
		// The command is passed as a single string for the remote shell to
		// parse — that is what lets the literal ~/docker expand remotely.
		argv = []string{"ssh", "-o", "BatchMode=yes", t.Dest, strings.Join(argv, " ")}
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

// remoteRepoPath is the tool's default repo location, assumed on every remote
// host (baked-in convention; no per-host override). Kept as a literal ~ so the
// REMOTE shell expands it.
const remoteRepoPath = "~/docker"

// resolveDeployTargets picks and orders the deploy targets. With no names,
// every host with a role is targeted: it runs a non-disabled service or is
// the dns_host (role-less spare hosts have nothing to apply). Explicit names
// restrict the set (unknown names are refused). Targets are sorted by name,
// except self — which always goes LAST (see cmdDeploy: bounce the resolver
// once, at the end).
func resolveDeployTargets(cfg *config.Config, self string, names []string) ([]deployTarget, error) {
	var picked []string
	if len(names) > 0 {
		seen := map[string]bool{}
		for _, n := range names {
			if _, ok := cfg.Hosts[n]; !ok {
				return nil, fmt.Errorf("unknown host %q — defined hosts: %s", n, strings.Join(sortedKeysOf(cfg.Hosts), ", "))
			}
			if !seen[n] {
				seen[n] = true
				picked = append(picked, n)
			}
		}
	} else {
		for name := range cfg.Hosts {
			if name == cfg.DNSHost() || hostRunsCaddy(cfg, name) {
				picked = append(picked, name)
			}
		}
	}
	sort.Strings(picked)
	out := make([]deployTarget, 0, len(picked))
	var selfTarget *deployTarget
	for _, n := range picked {
		t := deployTarget{Name: n, Dest: cfg.Hosts[n].SSHDest(n), Local: n == self}
		if t.Local {
			tt := t
			selfTarget = &tt
			continue
		}
		out = append(out, t)
	}
	if selfTarget != nil {
		out = append(out, *selfTarget)
	}
	return out, nil
}

// deployPreflight refuses a deploy from a dirty or unpushed local repo. The
// fleet pulls from origin, so a local-only change would deploy a state that
// does not match what this machine shows — origin carries the intent.
func deployPreflight(repoRoot string) error {
	out, err := exec.Command("git", "-C", repoRoot, "status", "--porcelain").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot check the repo's git status in %s: %v\n%s", repoRoot, err, strings.TrimSpace(string(out)))
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return fmt.Errorf("the local repo is dirty — commit and push before deploying:\n%s", s)
	}
	out, err = exec.Command("git", "-C", repoRoot, "rev-list", "@{u}..HEAD").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot compare HEAD against its upstream in %s (no upstream configured?): %v\n%s", repoRoot, err, strings.TrimSpace(string(out)))
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		n := len(strings.Split(s, "\n"))
		return fmt.Errorf("the local repo has %d unpushed %s — push before deploying", n, plural(n, "commit"))
	}
	return nil
}

// Per-target argv builders. A local target operates on the local checkout
// (repoRoot, which honors -C); remotes use the baked-in ~/docker convention.

func pullArgv(t deployTarget, localRepo string) []string {
	repo := remoteRepoPath
	if t.Local {
		repo = localRepo
	}
	return []string{"git", "-C", repo, "pull", "--ff-only"}
}

func headArgv(t deployTarget, localRepo string) []string {
	repo := remoteRepoPath
	if t.Local {
		repo = localRepo
	}
	return []string{"git", "-C", repo, "rev-parse", "HEAD"}
}

func applyArgv(t deployTarget, localRepo string) []string {
	if t.Local {
		// Re-invoke this same binary (hemma may not be on PATH in a dev build).
		exe, err := os.Executable()
		if err != nil {
			exe = "hemma"
		}
		return []string{exe, "-C", localRepo, "apply"}
	}
	return []string{"hemma", "apply"}
}

// runDeploy executes the two phases against resolved, ordered targets.
// Factored off cmdDeploy so the phase logic runs under a fake runner in tests.
func runDeploy(r deployRunner, targets []deployTarget, localRepo string) int {
	fmt.Printf("Deploying to %d %s: %s.\n", len(targets), plural(len(targets), "host"), joinTargetNames(targets))

	// Phase 1 — pull everywhere; ANY failure aborts the whole deploy.
	fmt.Printf("\n%s== Phase 1: pull ==%s\n", boldOn, boldOff)
	for _, t := range targets {
		fmt.Printf("\n%s== %s ==%s\n", boldOn, t.Name, boldOff)
		out, err := r.run(t, pullArgv(t, localRepo))
		printIndented(out)
		if err != nil {
			fmt.Println()
			errf("Pull failed on %q — deploy aborted, phase 2 (apply) never started.", t.Name)
			hint("Either the host is unreachable over ssh, or its checkout has diverged from")
			hint("origin (local commits or edits origin doesn't have). Hosts must not fork the")
			hint("repo — reconcile that host manually, then re-run 'hemma deploy'.")
			hint("No runtime state was changed anywhere: a pulled-but-not-applied checkout is")
			hint("the normal intermediate state of the manual pull-then-apply workflow.")
			return 1
		}
	}

	// All pulls succeeded — but a push may have raced between them, landing
	// hosts on different commits. Deploying a mixed fleet silently is worse
	// than re-running, so verify every HEAD matches.
	heads := make([]string, len(targets))
	for i, t := range targets {
		out, err := r.run(t, headArgv(t, localRepo))
		if err != nil {
			fmt.Println()
			printIndented(out)
			errf("rev-parse failed on %q — deploy aborted, phase 2 (apply) never started.", t.Name)
			hint("No runtime state was changed anywhere.")
			return 1
		}
		heads[i] = strings.TrimSpace(out)
	}
	for i := 1; i < len(heads); i++ {
		if heads[i] != heads[0] {
			fmt.Println()
			for j, t := range targets {
				fmt.Printf("  %s: %s\n", t.Name, heads[j])
			}
			fmt.Println()
			errf("Hosts pulled different commits — a push raced between the pulls. Re-run 'hemma deploy'.")
			hint("No runtime state was changed anywhere (nothing was applied).")
			return 1
		}
	}
	fmt.Printf("\nAll hosts at %s.\n", shortCommit(heads[0]))

	// Phase 2 — apply per host, remotes first, self LAST. A failure is
	// reported and the fan-out continues: each host's apply is internally
	// consistent, and stopping here would only leave a less converged fleet.
	fmt.Printf("\n%s== Phase 2: apply ==%s\n", boldOn, boldOff)
	status := make(map[string]string, len(targets))
	failed := 0
	for _, t := range targets {
		fmt.Printf("\n%s== %s ==%s\n", boldOn, t.Name, boldOff)
		out, err := r.run(t, applyArgv(t, localRepo))
		printIndented(out)
		if err != nil {
			fmt.Println("  " + cross + " apply FAILED")
			status[t.Name] = "failed-apply"
			failed++
			continue
		}
		status[t.Name] = "applied"
	}

	fmt.Printf("\n%s== Summary ==%s\n", boldOn, boldOff)
	for _, t := range targets {
		glyph := tick
		if status[t.Name] != "applied" {
			glyph = cross
		}
		fmt.Printf("  %s %s: %s\n", glyph, t.Name, status[t.Name])
	}
	fmt.Println()
	if failed > 0 {
		errf("Apply failed on %d %s — restarts cannot be rolled back; fix and re-run 'hemma deploy' (apply is idempotent).", failed, plural(failed, "host"))
		return 1
	}
	fmt.Println("Deployed.")
	return 0
}

// printIndented prints a command's captured output, indented under the host
// header; empty output prints nothing.
func printIndented(out string) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		fmt.Printf("  %s\n", line)
	}
}

func joinTargetNames(targets []deployTarget) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
		if t.Local {
			names[i] += " (here, last)"
		}
	}
	return strings.Join(names, ", ")
}

// shortCommit abbreviates a full commit hash for display.
func shortCommit(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
