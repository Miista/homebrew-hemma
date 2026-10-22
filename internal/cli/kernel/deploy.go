package kernel

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"hemma/internal/auth"
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
//	Phase 2 — validate auth:      the auth provider's config validate, on the auth host
//	Phase 3 — apply everywhere:   hemma apply   (remotes first, self LAST)
//
// Phase 0 aborts up front if any remote is unreachable over ssh, so a
// mis-wired host (missing key, unknown host key) fails before anything is
// pulled — a clear "unreachable" instead of git-pull noise mid-fan-out.
//
// Phase 1 is all-or-nothing: `pull --ff-only` either fast-forwards or refuses
// touching nothing, so ANY failure (divergence, dirty remote tree, unreachable
// host, ssh auth) aborts the entire deploy before phase 3 starts — no runtime
// state has changed anywhere. Pulled-but-not-applied checkouts are the normal
// intermediate state of the manual workflow, so an abort leaves nothing to
// undo. After the pulls, every host must be on the SAME commit (a push racing
// between pulls would silently deploy different states) — a mismatch also
// aborts.
//
// Phase 2 requires the auth host to hold a valid auth config: the provider's validate runs there (locally or over
// ssh) once every checkout is on the new commit, and a failure aborts the deploy before anything is applied. The auth
// provider is what signs people in, so a config it would refuse must never reach its restart — and must not cost the
// other hosts a reload either. `apply` still validates again before it restarts (that guard stays).
//
// Phase 3 is per-host best-effort: each host's `apply` is internally
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
// Deploying means "make the fleet match origin", so origin must carry the
// intent. On this (the deploy) host, deploy refuses a dirty tree, then brings
// the checkout level with origin itself: `git pull --rebase` when origin has
// work this checkout lacks, `git push` for unpushed commits (a rebase that hits
// a real conflict is aborted, nothing changed). It then checks that the
// generated files still match services.yaml — a rebase can leave them stale —
// and refuses, before any host is touched, if they do not.
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
	// A mistyped host name, or nothing to deploy, is settled from the config alone — before anything touches git.
	targets, err := resolveDeployTargets(cfg, self, args)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(env.Out, "Nothing to deploy: no host runs a service or is the dns_host.")
		return 0
	}
	// Sync with origin. This machine is the deploy host, and other hosts commit to the same repository too (an
	// updater on one of them, say), so origin having work this checkout lacks is ordinary, not an error for a person
	// to resolve. The sync can change services.yaml, and with it the hosts to deploy to, so the config is read again
	// afterwards.
	if err := deployPreflight(env, repoRoot); err != nil {
		errf(env, "%v", err)
		hint(env, "Deploy means \"make the fleet match origin\" — origin must carry the change.")
		return 1
	}
	if cfg, code = loadExisting(env, cfgPath, "deploy"); cfg == nil {
		return code
	}
	self = localHost(cfg)
	if driftRefusal(env, repoRoot, cfg) {
		return 1
	}
	if targets, err = resolveDeployTargets(cfg, self, args); err != nil {
		errf(env, "%v", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(env.Out, "Nothing to deploy: no host runs a service or is the dns_host.")
		return 0
	}
	// If a remote host turns out to hold commits origin lacks, they reach origin during phase 1 and this host must take
	// them in too: the same sync and drift check again.
	resync := func() error {
		if err := deployPreflight(env, repoRoot); err != nil {
			return err
		}
		c, _ := loadExisting(env, cfgPath, "deploy")
		if c == nil {
			return fmt.Errorf("services.yaml cannot be read after syncing with origin")
		}
		if driftRefusal(env, repoRoot, c) {
			return fmt.Errorf("the repo has drifted after syncing with origin — deploy stopped")
		}
		return nil
	}
	return runDeploy(env, execRunner{env}, targets, repoRoot, planAuthCheck(cfg), resync)
}

// driftRefusal reports (and says why) when the generated files no longer match services.yaml. A rebase can leave them
// stale — both sides regenerate the same files — and apply refuses a drifted repo on every host, so find out here,
// before any host pulls or restarts anything, instead of failing host by host in phase 3.
func driftRefusal(env *Env, repoRoot string, cfg *config.Config) bool {
	d := detectDrift(repoRoot, cfg, loadManifest(env, repoRoot, cfg))
	if !d.Any() {
		return false
	}
	errf(env, "The repo has drifted (%d %s out of sync with services.yaml) — deploy would be refused at apply on every host.",
		d.Count(), plural(d.Count(), "generated file"))
	printDriftDetail(env, d)
	fmt.Fprintln(env.Err)
	hint(env, "Run 'hemma doctor --fix', commit and push the result, then deploy again.")
	return true
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

// gitFunc runs one git command in a checkout — this machine's (directly) or a remote host's (over ssh) — and returns
// its trimmed combined output.
type gitFunc func(args ...string) (string, error)

// syncWho words the messages of syncWithOrigin for whose checkout it is.
type syncWho struct {
	owner string // "your" | "optiplex's"
	where string // "here" | "on optiplex"
}

var syncLocal = syncWho{"your", "here"}

// deployPreflight makes THIS checkout carry exactly what origin should: refuses a dirty tree (a local-only edit would
// deploy a state that does not match what this machine shows), then brings it level with origin (syncWithOrigin).
func deployPreflight(env *Env, repoRoot string) error {
	git := func(args ...string) (string, error) {
		out, err := env.command("git", append([]string{"-C", repoRoot}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	out, err := git("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("cannot check the repo's git status in %s: %v\n%s", repoRoot, err, out)
	}
	// Only hemma's own files must be committed: they are what goes live, so they must carry a commit message. Anything
	// else in the repo is somebody else's work in progress (see OwnedPaths) and is left alone, as on a remote host.
	owned := OwnedPaths(repoRoot, func(p string) ([]byte, bool) {
		b, err := git("show", "HEAD:"+p)
		return []byte(b), err == nil
	})
	var ours []string
	for _, line := range strings.Split(out, "\n") {
		if paths := porcelainPaths(line); len(paths) == 1 && owned[paths[0]] {
			ours = append(ours, line)
		}
	}
	if len(ours) > 0 {
		return fmt.Errorf("the local repo is dirty — commit before deploying:\n%s", strings.Join(ours, "\n"))
	}
	if out, err := git("rev-parse", "--abbrev-ref", "@{u}"); err != nil {
		return fmt.Errorf("cannot find the upstream of the current branch in %s (no upstream configured?): %v\n%s", repoRoot, err, out)
	}
	_, err = syncWithOrigin(git, syncLocal, true, func(format string, a ...any) { fmt.Fprintf(env.Out, format+"\n", a...) })
	return err
}

// syncWithOrigin brings one checkout level with origin: `git pull --rebase` when origin has commits it lacks,
// `git push` when it has commits origin lacks. It serves this host (a clean tree, see deployPreflight) and every
// remote host (reconcileRemote), so the two cannot drift apart. Other hosts commit to the same repository too (an
// updater on one of them, say), so "origin has work this checkout lacks" is ordinary, not an error for a person to
// resolve.
//
// A rebase is transactional: if it stops on a real conflict it is aborted, leaving the repo exactly as it was, and the
// error says so. With allowDirty (a remote host: someone may be mid-edit there) uncommitted files are left alone, and
// only stashed around the rebase when none of them is also changed by origin's new commits — then re-applying them
// cannot conflict. If one is, nothing is touched and the error names the files. Returns whether anything was pushed
// (so the caller knows origin gained commits).
func syncWithOrigin(git gitFunc, who syncWho, allowDirty bool, say func(format string, a ...any)) (pushed bool, err error) {
	// A few rounds: after a push, someone else's push can land in between and refuse it once more.
	for round := 0; round < 3; round++ {
		if out, err := git("fetch"); err != nil {
			return pushed, fmt.Errorf("cannot reach origin to compare with it (%s): %v\n%s", who.where, err, out)
		}
		ahead, behind, err := aheadBehind(git)
		if err != nil {
			return pushed, fmt.Errorf("cannot compare the checkout %s with origin (no upstream configured?): %v", who.where, err)
		}
		if behind > 0 {
			pullArgs := []string{"pull", "--rebase"}
			if allowDirty {
				status, _ := git("status", "--porcelain")
				if dirty := porcelainPaths(status); len(dirty) > 0 {
					incoming, _ := git("diff", "--name-only", "HEAD...@{u}")
					if overlap := intersect(dirty, strings.Fields(incoming)); len(overlap) > 0 {
						return pushed, fmt.Errorf("%s has uncommitted edits to %s, which origin's new commits also change — nothing was touched. "+
							"Commit or stash the edits there, then deploy again.", strings.TrimSuffix(who.owner, "'s"), strings.Join(overlap, ", "))
					}
					pullArgs = append(pullArgs, "--autostash")
				}
			}
			if out, err := git(pullArgs...); err != nil {
				git("rebase", "--abort") // a no-op unless the rebase stopped on a conflict
				return pushed, fmt.Errorf("origin has %d new %s that overlap with %s %d unpushed %s, so they could not be combined — nothing was changed %s.\n%s\n"+
					"Resolve it in the repo (git pull --rebase, fix the conflict), then deploy again.",
					behind, plural(behind, "commit"), who.owner, ahead, plural(ahead, "commit"), who.where, out)
			}
			if ahead > 0 {
				say("Rebased %s %d unpushed %s onto %d new from origin.", who.owner, ahead, plural(ahead, "commit"), behind)
			} else {
				say("Pulled %d new %s from origin %s.", behind, plural(behind, "commit"), who.where)
			}
		}
		if ahead == 0 {
			return pushed, nil
		}
		out, err := git("push")
		if err == nil {
			say("Pushed %s %d %s to origin.", who.owner, ahead, plural(ahead, "commit"))
			return true, nil
		}
		if !strings.Contains(out, "fetch first") && !strings.Contains(out, "non-fast-forward") && !strings.Contains(out, "rejected") {
			return pushed, fmt.Errorf("pushing %s %d unpushed %s to origin failed:\n%s", who.owner, ahead, plural(ahead, "commit"), out)
		}
		// origin moved again while we were pushing: take another round
	}
	return pushed, fmt.Errorf("origin kept changing while pushing — try again")
}

// reconcileRemote brings one REMOTE host's checkout level with origin before the fleet pulls (see syncWithOrigin): its
// own unpushed commits (the updater's, say) reach origin, and origin's new ones reach it. Uncommitted edits there are
// respected: see syncWithOrigin.
func reconcileRemote(env *Env, r deployRunner, t deployTarget) (pushed bool, err error) {
	git := func(args ...string) (string, error) {
		out, err := r.run(t, append([]string{"git", "-C", remoteRepoPath}, args...))
		return strings.TrimSpace(out), err
	}
	return syncWithOrigin(git, syncWho{owner: t.Name + "'s", where: "on " + t.Name}, true,
		func(format string, a ...any) { fmt.Fprintf(env.Out, "  "+format+"\n", a...) })
}

// aheadBehind counts the commits this checkout has that its upstream lacks (ahead) and the other way round (behind).
// Output that cannot be read counts as level (0, 0); a git failure is an error.
func aheadBehind(git gitFunc) (ahead, behind int, err error) {
	out, err := git("rev-list", "--left-right", "--count", "HEAD...@{u}")
	if err != nil {
		return 0, 0, fmt.Errorf("%v: %s", err, out)
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, nil
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// porcelainPaths reads the paths out of `git status --porcelain` (a rename counts as its new path). Each line is
// "XY path", where X may be a space — and the caller's trimming may have eaten the very first line's leading space — so
// the path is whatever follows the first space AFTER the status letters, not a fixed column.
func porcelainPaths(status string) []string {
	var paths []string
	for _, line := range strings.Split(status, "\n") {
		l := strings.TrimLeft(line, " ")
		i := strings.IndexByte(l, ' ')
		if i < 0 {
			continue
		}
		p := strings.TrimSpace(l[i+1:])
		if j := strings.LastIndex(p, " -> "); j >= 0 {
			p = p[j+4:]
		}
		if p != "" {
			paths = append(paths, strings.Trim(p, "\""))
		}
	}
	return paths
}

// intersect returns the members of a that are also in b, in a's order.
func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
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

// runDeploy executes the phases against resolved, ordered targets.
// Factored off cmdDeploy so the phase logic runs under a fake runner in tests.
func runDeploy(env *Env, r deployRunner, targets []deployTarget, localRepo string, auth *authCheck, resync func() error) int {
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

	// First bring every REMOTE checkout level with origin (syncWithOrigin): commits made on a remote host (an updater's,
	// say) reach origin, origin's new ones reach it, and edits someone has under way there are left alone. A remote whose
	// edits clash with origin's new commits stops the deploy here, naming the files. If any remote pushed, origin gained
	// commits this host lacks, so this host syncs again (and re-checks for drift) before everyone pulls.
	pushedFromRemote := false
	for _, t := range targets {
		if t.Local {
			continue
		}
		pushed, err := reconcileRemote(env, r, t)
		if err != nil {
			fmt.Fprintln(env.Out)
			errf(env, "Could not bring %q level with origin — deploy aborted, phase 3 (apply) never started.\n%v", t.Name, err)
			hint(env, "No runtime state was changed anywhere.")
			return 1
		}
		pushedFromRemote = pushedFromRemote || pushed
	}
	if pushedFromRemote && resync != nil {
		if err := resync(); err != nil {
			fmt.Fprintln(env.Out)
			errf(env, "%v", err)
			hint(env, "No runtime state was changed anywhere.")
			return 1
		}
	}

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
			errf(env, "Pull failed on %q — deploy aborted, phase 3 (apply) never started.", t.Name)
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
			errf(env, "rev-parse failed on %q — deploy aborted, phase 3 (apply) never started.", t.Name)
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

	// Phase 2 — the auth host must hold a valid auth config. Every checkout is on the new commit now, so the
	// provider validates what apply would restart it with; a failure aborts before ANY host is applied.
	fmt.Fprintf(env.Out, "\n%s== Phase 2: validate auth ==%s\n", boldOn, boldOff)
	if code := validateAuth(env, r, targets, auth); code != 0 {
		return code
	}

	// Phase 3 — apply per host, remotes first, self LAST. A failure is
	// reported and the fan-out continues: each host's apply is internally
	// consistent, and stopping here would only leave a less converged fleet.
	fmt.Fprintf(env.Out, "\n%s== Phase 3: apply ==%s\n", boldOn, boldOff)
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

// authCheck is the auth provider's config validation and the host it must run on.
type authCheck struct {
	Service  string   // the auth_service name (the provider's container)
	Host     string   // the host that runs it
	Validate []string // argv, run on Host
}

// planAuthCheck says what phase 2 validates: nil when no auth service is set, it is disabled, or its provider has
// nothing to validate. The host is the auth service's own host, which need not be the deploy host.
func planAuthCheck(cfg *config.Config) *authCheck {
	name := cfg.Defaults.AuthService
	s, ok := cfg.Services[name]
	if name == "" || !ok || s.Disabled {
		return nil
	}
	validate, _ := auth.Default().ApplyCommands(name)
	if validate == nil {
		return nil
	}
	return &authCheck{Service: name, Host: s.Host, Validate: validate}
}

// validateAuth runs the provider's validate on the auth host and returns the exit code to stop with (0: carry on). A
// deploy that does not target the auth host restarts nothing there, so there is nothing to hold back.
func validateAuth(env *Env, r deployRunner, targets []deployTarget, a *authCheck) int {
	if a == nil {
		fmt.Fprintln(env.Out, "  no auth service to validate")
		return 0
	}
	for _, t := range targets {
		if t.Name != a.Host {
			continue
		}
		out, err := r.run(t, a.Validate)
		if err != nil {
			fmt.Fprintf(env.Out, "\n%s== %s ==%s\n", boldOn, t.Name, boldOff)
			printIndented(env, out)
			fmt.Fprintln(env.Out, "  "+cross+" "+a.Service+" config validate FAILED")
			fmt.Fprintln(env.Out)
			errf(env, "The auth config on %q is not valid — deploy aborted, nothing was applied or restarted anywhere.", t.Name)
			return 1
		}
		fmt.Fprintf(env.Out, "  "+tick+" %s config validates on %s\n", a.Service, t.Name)
		return 0
	}
	fmt.Fprintf(env.Out, "  auth host %q is not part of this deploy — skipped\n", a.Host)
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
