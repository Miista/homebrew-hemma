package kernel

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/plan"
	"hemma/internal/render"
	syncpkg "hemma/internal/sync"
)

// planPaths returns the unique repo-relative paths a plan would write.
func planPaths(p *plan.Plan) []string {
	var paths []string
	seen := map[string]bool{}
	for _, files := range p.Files {
		for _, f := range files {
			if !seen[f.Path] {
				seen[f.Path] = true
				paths = append(paths, f.Path)
			}
		}
	}
	return paths
}

// warnIfIgnored prints a SHORT one-line warning to stderr if any of the plan's
// output paths are gitignored (they'd generate but never commit/deploy). It
// fires every run while the problem persists — a standing deploy hazard should
// stay visible — and points at `hemma doctor` for the full file list + fix.
// Silent when nothing is ignored or the check can't run (git absent / no repo).
func warnIfIgnored(env *Env, repoRoot string, p *plan.Plan) {
	ignored, ok := ignoredPaths(env, repoRoot, planPaths(p))
	if !ok || len(ignored) == 0 {
		return
	}
	noun := plural(len(ignored), "file")
	verb := "is"
	if len(ignored) != 1 {
		verb = "are"
	}
	fmt.Fprintf(env.Err,
		warn+" %d generated %s %s gitignored and won't deploy. Run 'hemma doctor --fix'.\n",
		len(ignored), noun, verb)
}

// printIgnoreDetail prints the full report: the ignored paths and the per-host
// .gitignore negation lines to add. Used by `hemma doctor`.
func printIgnoreDetail(env *Env, ignored []string) {
	fmt.Fprintf(env.Out, "%d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Fprintf(env.Out, "  %s\n", p)
	}
	fmt.Fprintln(env.Out, "\nAdd to .gitignore to un-ignore them (or run 'hemma doctor --fix'):")
	for _, rule := range unignoreRules() {
		fmt.Fprintf(env.Out, "  %s\n", rule)
	}
}

// cmdDoctor audits the repo for problems hemma can detect. Read-only by default;
// `hemma doctor --fix` applies the .gitignore negations (the one fix hemma can make
// safely). Exits non-zero if problems remain.
func cmdDoctor(env *Env, cfgPath string, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fixFlag := fs.Bool("fix", false, "apply fixes (write .gitignore entries)")
	fs.BoolVar(fixFlag, "f", false, "alias for --fix")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fix := *fixFlag

	repoRoot := filepath.Dir(cfgPath)
	cfg, code := loadExisting(env, cfgPath, "check")
	if cfg == nil {
		return code
	}
	// Load the auth snippet source so the plan below renders the real
	// (auth) file content, not the empty stub — otherwise the gitignore check's
	// plan is stale. On failure, warn and proceed (keep-last-good), the same way
	// runSync handles it — but count it as a problem (non-zero exit, design
	// §4.5/§8): a bad auth_snippet path is exactly what doctor exists to
	// surface. detectDrift/checkDrift load it independently, so this only
	// affects the top-level plan used for the gitignore check.
	problems := 0
	if authErr := cfg.LoadAuthSnippet(repoRoot); authErr != nil {
		errf(env, "auth_snippet unreadable — keeping the existing generated auth snippet: %v", authErr)
		problems++
	}
	p := plan.Build(cfg)

	// --- gitignore check ---
	ignored, ok := ignoredPaths(env, repoRoot, planPaths(p))
	if !ok {
		fmt.Fprintln(env.Out, "Skipped gitignore check (git not available or not a repository).")
	} else if len(ignored) == 0 {
		fmt.Fprintln(env.Out, tick+" No generated files are gitignored.")
	} else {
		problems++
		if fix {
			// fix: write the negation block to the repo-root .gitignore, then re-verify.
			gi := filepath.Join(repoRoot, ".gitignore")
			if err := writeManagedBlock(gi, unignoreRules()); err != nil {
				errf(env, "%v", err)
				return 1
			}
			fmt.Fprintln(env.Out, tick+" Updated .gitignore")
			still, ok := ignoredPaths(env, repoRoot, planPaths(p))
			if !ok {
				// can't re-check; assume the write was enough
			} else if len(still) == 0 {
				fmt.Fprintln(env.Out, tick+" All generated files are now tracked by git.")
				problems--
			} else {
				errf(env, "%d %s still gitignored after fix:", len(still), plural(len(still), "file"))
				for _, p := range still {
					fmt.Fprintf(env.Err, "  %s\n", p)
				}
			}
		} else {
			printIgnoreDetail(env, ignored)
			fmt.Fprintln(env.Out, "\nRun 'hemma doctor --fix' to add these entries automatically.")
		}
	}

	// --- managed block freshness (an older hemma wrote broader rules) ---
	problems += checkManagedBlock(env, filepath.Join(repoRoot, ".gitignore"), fix)

	// --- Caddyfile import line check ---
	problems += checkCaddyfileImports(env, repoRoot, cfg, fix)
	// Advisory, NOT counted toward problems: hemma cannot fix this one (the
	// global options block it belongs in is hand-maintained, §4.7), so
	// counting it would make `doctor --fix` permanently unable to exit 0 —
	// the same reasoning that keeps the access-control wiring check advisory.
	// Enforcement lives in `apply`, which hard-refuses rather than reload a
	// Caddy whose generated trust boundary is never loaded.
	checkTrustedProxiesImport(env, repoRoot, cfg, fix)

	// --- generated-file drift check (missing / modified / orphaned) ---
	problems += checkDrift(env, repoRoot, cfg, fix)

	// --- deploy ssh readiness (known_hosts + no-op probe per remote host) ---
	problems += checkDeployReadiness(env, cfg)

	// --- instructive advisories (auth config, users database, wiring) ---
	// Report-but-proceed: these don't corrupt generated files, but the
	// snippet-without-auth_service case reproduces the redirect-loop bug, so it
	// counts as a doctor problem (non-zero exit). The rest are advisory.
	// The users-db cross-checks are gated inside the provider on the users
	// database file existing; the wiring check on the access-control artifact
	// being part of the plan. None of them is --fix-able (hemma never writes
	// docker-compose.yml, configuration.yml, or the users database), so each
	// advisory carries the full paste-in recipe, rendered by printAdvisories,
	// and the block ends with one summary line saying --fix does not resolve
	// them — printed in --fix mode too, so surviving advisories don't read as
	// --fix having failed.
	advs := authConfigWarnings(repoRoot, cfg)
	if len(advs) > 0 {
		if cfg.Defaults.AuthSnippet != "" && cfg.Defaults.AuthService == "" {
			problems++
		}
	} else if cfg.Defaults.AuthSnippet != "" {
		fmt.Fprintln(env.Out, tick+" Auth config is consistent.")
	}
	advs = append(advs, usersDBWarnings(repoRoot, cfg)...)
	advs = append(advs, authWiringWarnings(repoRoot, cfg)...)
	// No separate public-horizon checks anymore (§12): hemma is now the sole
	// author of each host's cloudflared config.yml, so there is no foreign
	// hand-maintained file left for services.yaml to drift against — that
	// drift is exactly what checkDrift (above) already catches generically,
	// the same as any other generated file.
	if len(advs) > 0 {
		fmt.Fprintln(env.Out)
		printAdvisories(env, repoRoot, advs)
		fmt.Fprintf(env.Out, "\n%sThese advisories require manual edits (see their fix: lines) — 'hemma doctor --fix' does not resolve them.%s\n", dimOn, dimOff)
	}

	if problems > 0 {
		return 1
	}
	return 0
}

// authWiringWarnings runs the provider's read-only access-control wiring
// check for `doctor`: is the generated artifact (§4.6) actually loaded by the
// auth container per the auth host's docker-compose.yml? Mirrors
// usersDBWarnings' config-location logic; silent when the auth service or its
// host isn't resolvable (flagged elsewhere), when the auth service is
// disabled (apply skips its auth half too), or when the artifact isn't part
// of the plan (gated inside the provider).
func authWiringWarnings(repoRoot string, cfg *config.Config) []auth.Advisory {
	if cfg.Defaults.AuthService == "" {
		return nil
	}
	authSvc, ok := cfg.Services[cfg.Defaults.AuthService]
	if !ok || authSvc.Disabled {
		return nil
	}
	hostM, ok := cfg.Hosts[authSvc.Host]
	if !ok {
		return nil
	}
	hostDir := filepath.Join(repoRoot, hostM.ResolvedDir(authSvc.Host))
	var svcs []auth.Service
	for name, s := range cfg.Services {
		if !s.Auth.Mode.UsesProvider() || s.Disabled {
			continue
		}
		svcs = append(svcs, auth.Service{Name: name, FQDN: s.FQDN, Mode: string(s.Auth.Mode), Groups: s.Auth.Groups, BypassPaths: s.Auth.BypassPaths})
	}
	return auth.Default().ValidateWiring(hostDir, cfg.Defaults.AuthService, svcs)
}

// checkDrift reports (and with fix, repairs) drift between services.yaml and the
// generated files on disk. Repair is a full Complete reconcile: it rewrites
// missing/modified files and GCs orphaned tracked files — subsuming what the old
// `sync --complete` command did.
func checkDrift(env *Env, repoRoot string, cfg *config.Config, fix bool) int {
	mf := loadManifest(env, repoRoot, cfg)
	d := detectDrift(repoRoot, cfg, mf)
	if !d.Any() {
		fmt.Fprintln(env.Out, tick+" Generated files are in sync with services.yaml.")
		return 0
	}

	if !fix {
		fmt.Fprintf(env.Out, cross+" %d generated %s out of sync with services.yaml:\n", d.Count(), plural(d.Count(), "file"))
		printDriftDetail(env, d)
		fmt.Fprintln(env.Out, "\nRun 'hemma doctor --fix' to reconcile (regenerate missing/modified, remove orphaned).")
		return 1
	}

	// Fix: full reconcile in Complete mode — regenerates and GCs in one pass.
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	res, err := eng.Reconcile(plan.Build(cfg), syncpkg.Complete)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	n := len(res.Created) + len(res.Updated) + len(res.Deleted)
	fmt.Fprintf(env.Out, tick+" Reconciled %d generated %s (%d created, %d updated, %d removed).\n",
		n, plural(n, "file"), len(res.Created), len(res.Updated), len(res.Deleted))

	// Re-check to confirm the repo is now clean.
	if still := detectDrift(repoRoot, cfg, mf); still.Any() {
		errf(env, "Drift remains after fix (%d %s):", still.Count(), plural(still.Count(), "file"))
		printDriftDetail(env, still)
		return 1
	}
	return 0
}

// checkCaddyfileImports checks that each host's Caddyfile contains the line
// `import hemma.generated.caddy`. If fix is true, appends the line when
// missing — or rewrites a pre-rename import line (`import splitdns.generated.caddy`
// or `import sd.generated.caddy`) in place, since leaving it would break caddy
// once GC removes the old file.
func checkCaddyfileImports(env *Env, repoRoot string, cfg *config.Config, fix bool) int {
	const importLine = "import " + render.CaddyImportFilename
	// Pre-rename import lines, newest first (splitdns era, then sd era).
	legacyImportLines := []string{"import splitdns.generated.caddy", "import sd.generated.caddy"}
	problems := 0
	for hostName, h := range cfg.Hosts {
		cfPath := filepath.Join(repoRoot, h.ResolvedDir(hostName), config.DefaultCaddyDataDir, "Caddyfile")
		data, err := os.ReadFile(cfPath)
		if os.IsNotExist(err) {
			// No Caddyfile on this host — nothing to check.
			continue
		}
		if err != nil {
			fmt.Fprintf(env.Out, cross+" %s: cannot read Caddyfile: %v\n", hostName, err)
			problems++
			continue
		}
		if strings.Contains(string(data), importLine) {
			fmt.Fprintf(env.Out, tick+" %s: Caddyfile imports %s\n", hostName, render.CaddyImportFilename)
			continue
		}
		problems++
		var legacy string
		for _, l := range legacyImportLines {
			if strings.Contains(string(data), l) {
				legacy = l
				break
			}
		}
		if !fix {
			if legacy != "" {
				fmt.Fprintf(env.Out, cross+" %s: Caddyfile has outdated `%s`\n", hostName, legacy)
			} else {
				fmt.Fprintf(env.Out, cross+" %s: Caddyfile missing `%s`\n", hostName, importLine)
			}
			continue
		}
		content := string(data)
		if legacy != "" {
			content = strings.ReplaceAll(content, legacy, importLine)
		} else {
			// Append the import line, ensuring a trailing newline before it.
			if !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			content += "\n" + importLine + "\n"
		}
		if err := config.AtomicWrite(cfPath, []byte(content)); err != nil {
			fmt.Fprintf(env.Out, cross+" %s: could not fix Caddyfile: %v\n", hostName, err)
			continue
		}
		if legacy != "" {
			fmt.Fprintf(env.Out, tick+" %s: rewrote `%s` -> `%s`\n", hostName, legacy, importLine)
		} else {
			fmt.Fprintf(env.Out, tick+" %s: added `%s` to Caddyfile\n", hostName, importLine)
		}
		problems--
	}
	return problems
}

// checkTrustedProxiesImport checks that each host's Caddyfile contains the
// line `import hemma.trusted_proxies.generated.caddy`. Unlike
// checkCaddyfileImports, this is REPORT-ONLY — --fix never edits the
// Caddyfile for it. The import must sit inside a global `servers {}` options
// block (Caddy's trusted_proxies option is server-wide only, never valid in a
// site block), Caddy allows exactly one global options block per file, and
// that block is hand-maintained territory (it may already carry `debug` or
// anything else). Writing into it would mean parsing and rewriting Caddyfile
// syntax hemma does not own — and guessing wrong appends a second global
// block, which Caddy refuses to load. A one-time manual line per host is the
// cheaper, safer contract, so this check states what to add and stops there
// (an instructive advisory in the §6.4 two-tier sense, not a mechanical fix).
func checkTrustedProxiesImport(env *Env, repoRoot string, cfg *config.Config, fix bool) int {
	const importLine = "import " + render.TrustedProxiesFilename
	problems := 0
	for hostName, h := range cfg.Hosts {
		cfPath := filepath.Join(repoRoot, h.ResolvedDir(hostName), config.DefaultCaddyDataDir, "Caddyfile")
		data, err := os.ReadFile(cfPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fmt.Fprintf(env.Out, cross+" %s: cannot read Caddyfile: %v\n", hostName, err)
			problems++
			continue
		}
		if strings.Contains(string(data), importLine) {
			fmt.Fprintf(env.Out, tick+" %s: Caddyfile imports %s\n", hostName, render.TrustedProxiesFilename)
			continue
		}
		problems++
		fmt.Fprintf(env.Out, cross+" %s: Caddyfile missing `%s`\n", hostName, importLine)
		fmt.Fprintf(env.Out, "    add it inside your global options block (hemma does not edit that block):\n")
		fmt.Fprintf(env.Out, "      {\n          servers {\n              %s\n          }\n      }\n", importLine)
	}
	return problems
}

const (
	giBlockStart = "# >>> hemma managed >>>"
	giBlockEnd   = "# <<< hemma managed <<<"
)

// Pre-rename marker generations (splitdns era, sd era); rewritten to the new
// ones on the next write so the old block is replaced in place instead of a
// duplicate being appended.
var legacyGiBlockStarts = []string{"# >>> splitdns managed >>>", "# >>> sd managed >>>"}
var legacyGiBlockEnds = []string{"# <<< splitdns managed <<<", "# <<< sd managed <<<"}

// writeManagedBlock writes rules into path inside a marked hemma block,
// creating the file if absent and preserving any content outside the markers.
// Idempotent.
func writeManagedBlock(path string, rules []string) error {
	var existing string
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Migrate pre-rename markers so the block below replaces the old block.
	for _, m := range legacyGiBlockStarts {
		existing = strings.ReplaceAll(existing, m, giBlockStart)
	}
	for _, m := range legacyGiBlockEnds {
		existing = strings.ReplaceAll(existing, m, giBlockEnd)
	}

	block := giBlockStart + "\n" +
		"# hemma-generated config under data/ dirs the repo otherwise ignores.\n" +
		"# Only the directories hemma writes into are re-included; the rest of every data/ stays\n" +
		"# ignored and is never walked by git. Managed by 'hemma doctor --fix'; edit outside these markers.\n" +
		strings.Join(rules, "\n") + "\n" +
		giBlockEnd + "\n"

	var out string
	if s, e := strings.Index(existing, giBlockStart), strings.Index(existing, giBlockEnd); s >= 0 && e > s {
		// Replace the existing block, preserving everything around it.
		tail := existing[e+len(giBlockEnd):]
		tail = strings.TrimPrefix(tail, "\n")
		out = existing[:s] + block + tail
	} else if existing == "" {
		out = block
	} else {
		// Append, ensuring a separating newline.
		if !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		out = existing + "\n" + block
	}
	return config.AtomicWrite(path, []byte(out))
}

// managedBlockRules returns the rule lines inside hemma's managed block of a
// .gitignore (comments and blanks dropped), and whether the block exists.
func managedBlockRules(content string) (rules []string, found bool) {
	for _, m := range legacyGiBlockStarts {
		content = strings.ReplaceAll(content, m, giBlockStart)
	}
	for _, m := range legacyGiBlockEnds {
		content = strings.ReplaceAll(content, m, giBlockEnd)
	}
	s, e := strings.Index(content, giBlockStart), strings.Index(content, giBlockEnd)
	if s < 0 || e <= s {
		return nil, false
	}
	for _, line := range strings.Split(content[s+len(giBlockStart):e], "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	return rules, true
}

// blanketDataRules are the lines older hemma versions (and its predecessor) wrote:
// they un-ignore every directory under every data/, so git walks all runtime data on
// every `git status`. The current rules never contain them.
var blanketDataRules = map[string]bool{"!**/data/": true, "!**/data/**/": true}

// checkManagedBlock keeps hemma's own block current. When the block exists but is not
// what hemma would write now (an older hemma wrote broader rules), it is reported and
// `--fix` rewrites it in place — this is not about ignored files, so it also reaches
// repos whose generated files are already tracked. Blanket rules left OUTSIDE the block
// (the predecessor tool's) are only advised about: hemma does not edit what it does not
// own, so that advisory is not counted as a problem. Returns the number of problems.
func checkManagedBlock(env *Env, giPath string, fix bool) int {
	b, err := os.ReadFile(giPath)
	if err != nil {
		return 0
	}
	content := string(b)
	problems := 0
	have, found := managedBlockRules(content)
	want := unignoreRules()
	if found && strings.Join(have, "\n") != strings.Join(want, "\n") {
		if fix {
			if err := writeManagedBlock(giPath, want); err != nil {
				errf(env, "%v", err)
				return 1
			}
			fmt.Fprintln(env.Out, tick+" Updated hemma's .gitignore block to the current (narrow) rules.")
		} else {
			problems++
			fmt.Fprintln(env.Out, warn+" hemma's .gitignore block is out of date: this version of hemma writes narrower rules and no longer re-includes whole data/ directories.")
			fmt.Fprintln(env.Out, "  Run 'hemma doctor --fix' to rewrite it.")
		}
	} else if found {
		fmt.Fprintln(env.Out, tick+" .gitignore block is current.")
	}
	// Blanket rules outside hemma's block.
	if s, e := strings.Index(content, giBlockStart), strings.Index(content, giBlockEnd); s >= 0 && e > s {
		content = content[:s] + content[e+len(giBlockEnd):]
	}
	var stray []string
	for _, line := range strings.Split(content, "\n") {
		if l := strings.TrimSpace(line); blanketDataRules[l] {
			stray = append(stray, l)
		}
	}
	if len(stray) > 0 {
		fmt.Fprintf(env.Out, warn+" .gitignore has blanket data/ rules outside hemma's block (%s): they make git walk all runtime data on every 'git status'. Remove them — hemma's narrow rules replace them.\n",
			strings.Join(dedupe(stray), ", "))
	}
	return problems
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ignoredPaths returns the subset of repo-relative paths that git would ignore
// in repoRoot, using git's own logic (git check-ignore). It returns (nil, false)
// when the check can't run — git missing, or repoRoot isn't a work tree — so
// callers can skip the warning rather than guess.
func ignoredPaths(env *Env, repoRoot string, relPaths []string) (ignored []string, ok bool) {
	if len(relPaths) == 0 {
		return nil, true
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	// `git check-ignore --stdin` prints the paths it would ignore, one per line.
	cmd := env.command("git", "-C", repoRoot, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(relPaths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 = "nothing ignored" (not an error for us). Any other
		// failure (not a repo, etc.) → can't determine; signal not-ok.
		if ee, isExit := err.(*exec.ExitError); isExit && ee.ExitCode() == 1 {
			return nil, true
		}
		return nil, false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			ignored = append(ignored, s)
		}
	}
	sort.Strings(ignored)
	return ignored, true
}

// unignoreRules returns the repo-root .gitignore negation block that
// re-includes hemma's generated files when a broad rule like **/data/** would
// otherwise ignore them.
//
// Git won't re-include a file under an excluded directory, so each directory
// between a data/ dir and a generated file is un-ignored first, then only the
// generated files in it are re-included — runtime data (.db, caches, …) stays
// ignored. The rules are deliberately NARROW: a blanket "!**/data/**/" would
// un-ignore every directory under every data/, and git would then walk all of
// the runtime data on every `git status` (thousands of files, seconds on a Pi,
// and `git status` is what the web UI runs on each page load). Here git only
// descends the few directories hemma writes into and skips the rest.
//
// The block is FIXED, not derived from what the current config happens to
// generate: it is the same in every repo and at every moment, so it covers a
// kind of output from the start (before the first service, or the first auth
// service, exists), never changes with the config, and needs no write unless
// hemma itself changes. It is built from the same constants the plan uses for
// its output paths, plus each registered auth provider's own config directory
// (where its generated artifact sits); TestUnignoreRules_CoversEveryPlanOutput
// fails if hemma ever writes under a data/ directory these rules do not cover.
func unignoreRules() []string {
	// One representative file per kind of output. Only the directory and the
	// file's marker (".generated.<ext>", or the extension) matter.
	shapes := []string{
		path.Join(config.DefaultCaddyDataDir, render.CaddyImportFilename),
		path.Join(config.DefaultCaddySitesDir, "service.caddy"),
		path.Join(config.DefaultCaddyTLSDir, render.TLSSnippetName("domain")+".caddy"),
		path.Join(config.DefaultDnsmasqDir, "service.generated.conf"),
	}
	for _, name := range auth.Names() {
		if prov, ok := auth.Lookup(name); ok {
			shapes = append(shapes, path.Join(path.Dir(prov.ConfigPath()), "artifact.generated.yml"))
		}
	}
	return rulesForOutputs(shapes)
}

// rulesForOutputs turns output file paths into narrow, host-agnostic negation
// rules: described from the service directory in front of data/ (caddy, pihole,
// authelia) onward, so one block covers every host. A file is matched by what
// marks it as generated — its ".generated.<ext>" tail, or else (caddy site
// files carry no marker) its extension. Paths with no data/ segment need no
// rule. The result is sorted and deduplicated, so it is stable.
func rulesForOutputs(outputPaths []string) []string {
	set := map[string]bool{}
	for _, p := range outputPaths {
		segs := strings.Split(filepath.ToSlash(p), "/")
		di := -1
		for i, s := range segs {
			if s == "data" && i > 0 && i < len(segs)-1 {
				di = i
				break
			}
		}
		if di < 0 {
			continue
		}
		base := "!**/" + segs[di-1] + "/data/"
		file := segs[len(segs)-1]
		dirs := segs[di+1 : len(segs)-1]
		for k := 1; k <= len(dirs); k++ {
			set[base+strings.Join(dirs[:k], "/")+"/"] = true
		}
		match := file
		if i := strings.Index(file, ".generated."); i >= 0 {
			match = "*" + file[i:]
		} else if ext := filepath.Ext(file); ext != "" {
			match = "*" + ext
		}
		if len(dirs) > 0 {
			match = strings.Join(dirs, "/") + "/" + match
		}
		set[base+match] = true
	}
	rules := make([]string, 0, len(set))
	for r := range set {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	return rules
}
