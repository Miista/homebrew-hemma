package kernel

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
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
// the same way. It runs in three strictly ordered phases:
//
//	Phase 0 — probe reachability: ssh <dest> true  (remotes only)
//	Phase 1 — pull everywhere:    git -C ~/docker pull --ff-only
//	Phase 2 — apply everywhere:   hemma apply   (remotes first, self LAST)
//
// Phase 0 aborts up front if any remote is unreachable over ssh, so a
// mis-wired host (missing key, unknown host key) fails before anything is
// pulled — a clear "unreachable" instead of git-pull noise mid-fan-out.
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
func cmdDeploy(env *Env, repoRoot, cfgPath string, args []string) int {
	cfg, code := loadExisting(env, cfgPath, "deploy")
	if cfg == nil {
		return code
	}
	self := localHost(cfg)
	// deploy_host names the ONE machine allowed to fan out. Refuse anywhere
	// else, before the preflight and before any ssh: on a segmented network a
	// non-origin host may have no route to its peers, so what would otherwise
	// happen is a partial deploy that dies host-by-host on connection timeouts,
	// leaving the fleet in a mixed state. Better to decline up front.
	//
	// A machine that matches NO declared host (a laptop with a checkout, say)
	// is also refused: it demonstrably is not the deploy host. Unset leaves
	// deploy runnable from anywhere, so this is opt-in.
	if dh := cfg.Defaults.DeployHost; dh != "" && self != dh {
		where := "this machine (it matches no declared host)"
		if self != "" {
			where = fmt.Sprintf("%q", self)
		}
		errf(env, "deploy_host is %q — refusing to deploy from %s.", dh, where)
		hint(env, "Run 'hemma deploy' on %s, or drop the restriction with 'hemma defaults set deploy-host -'.", dh)
		return 1
	}
	targets, err := resolveDeployTargets(cfg, self, args)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(env.Out, "Nothing to deploy: no host runs a service or is the dns_host.")
		return 0
	}
	if err := deployPreflight(env, repoRoot); err != nil {
		errf(env, "%v", err)
		hint(env, "Deploy means \"make the fleet match origin\" — origin must already carry the change.")
		return 1
	}
	return runDeploy(env, execRunner{env}, targets, repoRoot)
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
type execRunner struct{ env *Env }

func (r execRunner) run(t deployTarget, argv []string) (string, error) {
	if t.Local && argv[0] == inProcessApply {
		return r.applyLocal(argv[2])
	}
	if !t.Local {
		// The command is passed as a single string for the remote shell to
		// parse — that is what lets the literal ~/docker expand remotely.
		argv = []string{"ssh", "-o", "BatchMode=yes", t.Dest, strings.Join(argv, " ")}
	}
	out, err := r.env.command(argv[0], argv[1:]...).CombinedOutput()
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
func deployPreflight(env *Env, repoRoot string) error {
	out, err := env.command("git", "-C", repoRoot, "status", "--porcelain").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot check the repo's git status in %s: %v\n%s", repoRoot, err, strings.TrimSpace(string(out)))
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return fmt.Errorf("the local repo is dirty — commit and push before deploying:\n%s", s)
	}
	out, err = env.command("git", "-C", repoRoot, "rev-list", "@{u}..HEAD").CombinedOutput()
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

// probeArgv is Phase 0's no-op reachability check: `true` exits 0 on any host,
// so the only way it fails is the ssh transport itself (auth, host key,
// unreachable). Local targets are never probed (they don't ssh).
func probeArgv(deployTarget, string) []string {
	return []string{"true"}
}

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

// inProcessApply stands in for the program name of this host's own apply: it is
// run by the kernel itself (see execRunner.run), not as a child process, so
// deploy works the same from the CLI, the web UI or any other front end and
// never depends on a hemma binary being installed.
const inProcessApply = "<in-process hemma>"

func applyArgv(t deployTarget, localRepo string) []string {
	if t.Local {
		return []string{inProcessApply, "-C", localRepo, "apply"}
	}
	return []string{"hemma", "apply"}
}

// ansiEscape matches the colour codes glyphs may carry when the process's
// stdout is a terminal; a child process writing to a pipe never emitted them,
// so the in-process apply's captured output is stripped to match.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// applyLocal runs this host's own apply in-process and returns its combined
// output, as a child process's CombinedOutput would have.
func (r execRunner) applyLocal(localRepo string) (string, error) {
	var buf bytes.Buffer
	sub := &Env{Out: &buf, Err: &buf, Ctx: r.env.Ctx, NoTerminal: true}
	code := cmdApply(sub, localRepo, filepath.Join(localRepo, configName), nil)
	out := ansiEscape.ReplaceAllString(buf.String(), "")
	if code != 0 {
		return out, fmt.Errorf("apply exited %d", code)
	}
	return out, nil
}

// summarizePull condenses successful `git pull --ff-only` output to one
// phrase: "up to date", or "pulled <range> — <shortstat>" when it moved.
// Unrecognized (but successful) output passes through first-line-only, so a
// git wording change degrades to terse rather than noisy.
func summarizePull(out string) string {
	if strings.Contains(out, "Already up to date") {
		return "up to date"
	}
	var rng, stat string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Updating "); ok {
			rng = v
		}
		if strings.Contains(line, "changed") && (strings.Contains(line, "insertion") || strings.Contains(line, "deletion") || strings.HasSuffix(line, "changed")) {
			stat = line
		}
	}
	if rng != "" && stat != "" {
		return "pulled " + rng + " — " + stat
	}
	if rng != "" {
		return "pulled " + rng
	}
	if first := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]); first != "" {
		return first
	}
	return "pulled"
}

// deployProbe is Phase 0: ssh a no-op to every remote target and abort the
// whole deploy if any is unreachable, before Phase 1 pulls anything. Returns 0
// to proceed, 1 to abort. Local targets are skipped (they never ssh).
func deployProbe(env *Env, r deployRunner, targets []deployTarget) int {
	fmt.Fprintf(env.Out, "\n%s== Phase 0: probe ==%s\n", boldOn, boldOff)
	unreachable := false
	for _, t := range targets {
		if t.Local {
			fmt.Fprintf(env.Out, "  "+tick+" %s: local (no ssh)\n", t.Name)
			continue
		}
		out, err := r.run(t, probeArgv(t, ""))
		if err == nil {
			fmt.Fprintf(env.Out, "  "+tick+" %s: reachable\n", t.Name)
			continue
		}
		fmt.Fprintf(env.Out, "  "+cross+" %s: unreachable\n", t.Name)
		printIndented(env, out)
		unreachable = true
	}
	if unreachable {
		fmt.Fprintln(env.Out)
		errf(env, "One or more hosts are unreachable over ssh — deploy aborted, nothing pulled or applied.")
		hint(env, "deploy uses 'ssh -o BatchMode=yes <dest>' (no interactive prompts), so each host")
		hint(env, "needs non-interactive key auth from here and an entry in known_hosts. Check the")
		hint(env, "host's 'ssh:' field in services.yaml, then verify: ssh -o BatchMode=yes <dest> true")
		return 1
	}
	return 0
}

// runDeploy executes the two phases against resolved, ordered targets.
// Factored off cmdDeploy so the phase logic runs under a fake runner in tests.
func runDeploy(env *Env, r deployRunner, targets []deployTarget, localRepo string) int {
	fmt.Fprintf(env.Out, "Deploying to %d %s: %s.\n", len(targets), plural(len(targets), "host"), joinTargetNames(targets))

	// Phase 0 — probe ssh reachability to every REMOTE target; ANY failure
	// aborts before a single host pulls. Phase 1's first per-host command
	// already SSHes, so this catches nothing Phase 1 wouldn't — but it moves
	// the abort earlier (nothing pulled anywhere, not even the first host) and
	// turns a transport failure into a clear "unreachable" message instead of
	// git-pull noise. Self is Local (no ssh), so it is not probed.
	if code := deployProbe(env, r, targets); code != 0 {
		return code
	}

	// Phase 1 — pull everywhere; ANY failure aborts the whole deploy.
	// Success is one tick line per host (apply-style); the raw git output is
	// shown only on failure, where it is the diagnostic.
	fmt.Fprintf(env.Out, "\n%s== Phase 1: pull ==%s\n", boldOn, boldOff)
	for _, t := range targets {
		out, err := r.run(t, pullArgv(t, localRepo))
		if err == nil {
			fmt.Fprintf(env.Out, "  "+tick+" %s: %s\n", t.Name, summarizePull(out))
			continue
		}
		fmt.Fprintf(env.Out, "\n%s== %s ==%s\n", boldOn, t.Name, boldOff)
		printIndented(env, out)
		{
			fmt.Fprintln(env.Out)
			errf(env, "Pull failed on %q — deploy aborted, phase 2 (apply) never started.", t.Name)
			hint(env, "Either the host is unreachable over ssh, or its checkout has diverged from")
			hint(env, "origin (local commits or edits origin doesn't have). Hosts must not fork the")
			hint(env, "repo — reconcile that host manually, then re-run 'hemma deploy'.")
			hint(env, "No runtime state was changed anywhere: a pulled-but-not-applied checkout is")
			hint(env, "the normal intermediate state of the manual pull-then-apply workflow.")
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
			fmt.Fprintln(env.Out)
			printIndented(env, out)
			errf(env, "rev-parse failed on %q — deploy aborted, phase 2 (apply) never started.", t.Name)
			hint(env, "No runtime state was changed anywhere.")
			return 1
		}
		heads[i] = strings.TrimSpace(out)
	}
	for i := 1; i < len(heads); i++ {
		if heads[i] != heads[0] {
			fmt.Fprintln(env.Out)
			for j, t := range targets {
				fmt.Fprintf(env.Out, "  %s: %s\n", t.Name, heads[j])
			}
			fmt.Fprintln(env.Out)
			errf(env, "Hosts pulled different commits — a push raced between the pulls. Re-run 'hemma deploy'.")
			hint(env, "No runtime state was changed anywhere (nothing was applied).")
			return 1
		}
	}
	fmt.Fprintf(env.Out, "  all hosts at %s\n", shortCommit(heads[0]))

	// Phase 2 — apply per host, remotes first, self LAST. A failure is
	// reported and the fan-out continues: each host's apply is internally
	// consistent, and stopping here would only leave a less converged fleet.
	fmt.Fprintf(env.Out, "\n%s== Phase 2: apply ==%s\n", boldOn, boldOff)
	status := make(map[string]string, len(targets))
	failed := 0
	for _, t := range targets {
		fmt.Fprintf(env.Out, "\n%s== %s ==%s\n", boldOn, t.Name, boldOff)
		out, err := r.run(t, applyArgv(t, localRepo))
		printIndented(env, out)
		if err != nil {
			fmt.Fprintln(env.Out, "  "+cross+" apply FAILED")
			status[t.Name] = "failed-apply"
			failed++
			continue
		}
		status[t.Name] = "applied"
	}

	fmt.Fprintf(env.Out, "\n%s== Summary ==%s\n", boldOn, boldOff)
	for _, t := range targets {
		glyph := tick
		if status[t.Name] != "applied" {
			glyph = cross
		}
		fmt.Fprintf(env.Out, "  %s %s: %s\n", glyph, t.Name, status[t.Name])
	}
	fmt.Fprintln(env.Out)
	if failed > 0 {
		errf(env, "Apply failed on %d %s — restarts cannot be rolled back; fix and re-run 'hemma deploy' (apply is idempotent).", failed, plural(failed, "host"))
		return 1
	}
	fmt.Fprintln(env.Out, "Deployed.")
	return 0
}

// printIndented prints a command's captured output, indented under the host
// header; empty output prints nothing.
func printIndented(env *Env, out string) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		fmt.Fprintf(env.Out, "  %s\n", line)
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
