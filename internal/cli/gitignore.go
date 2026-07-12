package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
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
func warnIfIgnored(repoRoot string, p *plan.Plan) {
	ignored, ok := ignoredPaths(repoRoot, planPaths(p))
	if !ok || len(ignored) == 0 {
		return
	}
	noun := plural(len(ignored), "file")
	verb := "is"
	if len(ignored) != 1 {
		verb = "are"
	}
	fmt.Fprintf(os.Stderr,
		warn+" %d generated %s %s gitignored and won't deploy. Run 'hemma doctor --fix'.\n",
		len(ignored), noun, verb)
}

// printIgnoreDetail prints the full report: the ignored paths and the per-host
// .gitignore negation lines to add. Used by `hemma doctor`.
func printIgnoreDetail(ignored []string) {
	fmt.Printf("%d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\nAdd to .gitignore to un-ignore them (or run 'hemma doctor --fix'):")
	for _, rule := range unignoreRules() {
		fmt.Printf("  %s\n", rule)
	}
}

// cmdDoctor audits the repo for problems hemma can detect. Read-only by default;
// `hemma doctor --fix` applies the .gitignore negations (the one fix hemma can make
// safely). Exits non-zero if problems remain.
func cmdDoctor(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fixFlag := fs.Bool("fix", false, "apply fixes (write .gitignore entries)")
	fs.BoolVar(fixFlag, "f", false, "alias for --fix")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fix := *fixFlag

	repoRoot := filepath.Dir(cfgPath)
	cfg, code := loadExisting(cfgPath, "check")
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
		errf("auth_snippet unreadable — keeping the existing generated auth snippet: %v", authErr)
		problems++
	}
	p := plan.Build(cfg)

	// --- gitignore check ---
	ignored, ok := ignoredPaths(repoRoot, planPaths(p))
	if !ok {
		fmt.Println("Skipped gitignore check (git not available or not a repository).")
	} else if len(ignored) == 0 {
		fmt.Println(tick + " No generated files are gitignored.")
	} else {
		problems++
		if fix {
			// fix: write the negation block to the repo-root .gitignore, then re-verify.
			gi := filepath.Join(repoRoot, ".gitignore")
			if err := writeManagedBlock(gi, unignoreRules()); err != nil {
				errf("%v", err)
				return 1
			}
			fmt.Println(tick + " Updated .gitignore")
			still, ok := ignoredPaths(repoRoot, planPaths(p))
			if !ok {
				// can't re-check; assume the write was enough
			} else if len(still) == 0 {
				fmt.Println(tick + " All generated files are now tracked by git.")
				problems--
			} else {
				errf("%d %s still gitignored after fix:", len(still), plural(len(still), "file"))
				for _, p := range still {
					fmt.Fprintf(os.Stderr, "  %s\n", p)
				}
			}
		} else {
			printIgnoreDetail(ignored)
			fmt.Println("\nRun 'hemma doctor --fix' to add these entries automatically.")
		}
	}

	// --- Caddyfile import line check ---
	problems += checkCaddyfileImports(repoRoot, cfg, fix)

	// --- generated-file drift check (missing / modified / orphaned) ---
	problems += checkDrift(repoRoot, cfg, fix)

	// --- auth config advisories ---
	// Report-but-proceed: these don't corrupt generated files, but the
	// snippet-without-auth_service case reproduces the redirect-loop bug, so it
	// counts as a doctor problem (non-zero exit). The rest are advisory.
	if warns := authConfigWarnings(repoRoot, cfg); len(warns) > 0 {
		for _, msg := range warns {
			fmt.Printf("%s %s\n", warn, msg)
		}
		if cfg.Defaults.AuthSnippet != "" && cfg.Defaults.AuthService == "" {
			problems++
		}
	} else if cfg.Defaults.AuthSnippet != "" {
		fmt.Println(tick + " Auth config is consistent.")
	}

	// --- users-database cross-checks (advisory only; never affect exit code) ---
	// Gated inside the provider on the users database file existing.
	for _, msg := range usersDBWarnings(repoRoot, cfg) {
		fmt.Printf("%s %s\n", warn, msg)
	}

	// --- access-control wiring check (advisory only; never affects exit code) ---
	// Gated on the access-control artifact being part of the plan; silent
	// otherwise. Not --fix-able (hemma never writes docker-compose.yml or
	// configuration.yml), so the warnings carry the full paste-in recipe.
	for _, msg := range authWiringWarnings(repoRoot, cfg) {
		fmt.Printf("%s %s\n", warn, msg)
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
func authWiringWarnings(repoRoot string, cfg *config.Config) []string {
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
		if s.Auth.Mode == config.AuthNone || s.Disabled {
			continue
		}
		svcs = append(svcs, auth.Service{Name: name, FQDN: s.FQDN, Mode: string(s.Auth.Mode), Groups: s.Auth.Groups, PublicPaths: s.PublicPaths})
	}
	return auth.Default().ValidateWiring(hostDir, cfg.Defaults.AuthService, svcs)
}

// checkDrift reports (and with fix, repairs) drift between services.yaml and the
// generated files on disk. Repair is a full Complete reconcile: it rewrites
// missing/modified files and GCs orphaned tracked files — subsuming what the old
// `sync --complete` command did.
func checkDrift(repoRoot string, cfg *config.Config, fix bool) int {
	mf := loadManifest(repoRoot, cfg)
	d := detectDrift(repoRoot, cfg, mf)
	if !d.Any() {
		fmt.Println(tick + " Generated files are in sync with services.yaml.")
		return 0
	}

	if !fix {
		fmt.Printf(cross+" %d generated %s out of sync with services.yaml:\n", d.Count(), plural(d.Count(), "file"))
		printDriftDetail(d)
		fmt.Println("\nRun 'hemma doctor --fix' to reconcile (regenerate missing/modified, remove orphaned).")
		return 1
	}

	// Fix: full reconcile in Complete mode — regenerates and GCs in one pass.
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	res, err := eng.Reconcile(plan.Build(cfg), syncpkg.Complete)
	if err != nil {
		errf("%v", err)
		return 1
	}
	n := len(res.Created) + len(res.Updated) + len(res.Deleted)
	fmt.Printf(tick+" Reconciled %d generated %s (%d created, %d updated, %d removed).\n",
		n, plural(n, "file"), len(res.Created), len(res.Updated), len(res.Deleted))

	// Re-check to confirm the repo is now clean.
	if still := detectDrift(repoRoot, cfg, mf); still.Any() {
		errf("Drift remains after fix (%d %s):", still.Count(), plural(still.Count(), "file"))
		printDriftDetail(still)
		return 1
	}
	return 0
}

// checkCaddyfileImports checks that each host's Caddyfile contains the line
// `import hemma.generated.caddy`. If fix is true, appends the line when
// missing — or rewrites a pre-rename import line (`import splitdns.generated.caddy`
// or `import sd.generated.caddy`) in place, since leaving it would break caddy
// once GC removes the old file.
func checkCaddyfileImports(repoRoot string, cfg *config.Config, fix bool) int {
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
			fmt.Printf(cross+" %s: cannot read Caddyfile: %v\n", hostName, err)
			problems++
			continue
		}
		if strings.Contains(string(data), importLine) {
			fmt.Printf(tick+" %s: Caddyfile imports %s\n", hostName, render.CaddyImportFilename)
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
				fmt.Printf(cross+" %s: Caddyfile has outdated `%s`\n", hostName, legacy)
			} else {
				fmt.Printf(cross+" %s: Caddyfile missing `%s`\n", hostName, importLine)
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
			fmt.Printf(cross+" %s: could not fix Caddyfile: %v\n", hostName, err)
			continue
		}
		if legacy != "" {
			fmt.Printf(tick+" %s: rewrote `%s` -> `%s`\n", hostName, legacy, importLine)
		} else {
			fmt.Printf(tick+" %s: added `%s` to Caddyfile\n", hostName, importLine)
		}
		problems--
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
		"# Managed by 'hemma doctor --fix'; edit outside these markers.\n" +
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

// ignoredPaths returns the subset of repo-relative paths that git would ignore
// in repoRoot, using git's own logic (git check-ignore). It returns (nil, false)
// when the check can't run — git missing, or repoRoot isn't a work tree — so
// callers can skip the warning rather than guess.
func ignoredPaths(repoRoot string, relPaths []string) (ignored []string, ok bool) {
	if len(relPaths) == 0 {
		return nil, true
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	// `git check-ignore --stdin` prints the paths it would ignore, one per line.
	cmd := exec.Command("git", "-C", repoRoot, "check-ignore", "--stdin")
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
// otherwise ignore them. Git won't re-include a file under an excluded
// directory, so the directories must be un-ignored first (lines 1–2); then
// only hemma's outputs are re-included — runtime data (.db, caches,
// certs, …) stays ignored. The .caddy rule is scoped to caddy data dirs
// (site files like pihole.caddy carry no .generated marker, so the extension
// alone would be too broad elsewhere); the .generated.yml rule covers the
// auth provider's access-control artifact. Host-agnostic; one block at the
// repo root.
func unignoreRules() []string {
	return []string{
		"!**/data/",
		"!**/data/**/",
		"!**/data/**/*.generated.conf",
		"!**/caddy/data/**/*.caddy",
		"!**/data/**/*.generated.yml",
	}
}
