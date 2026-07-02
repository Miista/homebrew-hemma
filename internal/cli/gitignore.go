package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"splitdns/internal/config"
	"splitdns/internal/plan"
	syncpkg "splitdns/internal/sync"
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
// stay visible — and points at `splitdns doctor` for the full file list + fix.
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
		warn+" %d generated %s %s gitignored and won't deploy. Run 'splitdns doctor --fix'.\n",
		len(ignored), noun, verb)
}

// printIgnoreDetail prints the full report: the ignored paths and the per-host
// .gitignore negation lines to add. Used by `splitdns doctor`.
func printIgnoreDetail(ignored []string) {
	fmt.Printf("%d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\nAdd to .gitignore to un-ignore them (or run 'splitdns doctor --fix'):")
	for _, rule := range unignoreRules() {
		fmt.Printf("  %s\n", rule)
	}
}

// cmdDoctor audits the repo for problems splitdns can detect. Read-only by default;
// `splitdns doctor --fix` applies the .gitignore negations (the one fix splitdns can make
// safely). Exits non-zero if problems remain.
func cmdDoctor(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fixFlag := fs.Bool("fix", false, "apply fixes (write .gitignore entries)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fix := *fixFlag

	repoRoot := filepath.Dir(cfgPath)
	cfg, code := loadExisting(cfgPath, "check")
	if cfg == nil {
		return code
	}
	p := plan.Build(cfg)

	problems := 0

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
			fmt.Println("\nRun 'splitdns doctor --fix' to add these entries automatically.")
		}
	}

	// --- Caddyfile import line check ---
	problems += checkCaddyfileImports(repoRoot, cfg, fix)

	// --- generated-file drift check (missing / modified / orphaned) ---
	problems += checkDrift(repoRoot, cfg, fix)

	if problems > 0 {
		return 1
	}
	return 0
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
		fmt.Println("\nRun 'splitdns doctor --fix' to reconcile (regenerate missing/modified, remove orphaned).")
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
// `import sd.generated.caddy`. If fix is true, appends the line when missing.
func checkCaddyfileImports(repoRoot string, cfg *config.Config, fix bool) int {
	const importLine = "import sd.generated.caddy"
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
			fmt.Printf(tick+" %s: Caddyfile imports sd.generated.caddy\n", hostName)
			continue
		}
		problems++
		if !fix {
			fmt.Printf(cross+" %s: Caddyfile missing `%s`\n", hostName, importLine)
			continue
		}
		// Append the import line, ensuring a trailing newline before it.
		content := string(data)
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "\n" + importLine + "\n"
		if err := config.AtomicWrite(cfPath, []byte(content)); err != nil {
			fmt.Printf(cross+" %s: could not fix Caddyfile: %v\n", hostName, err)
			continue
		}
		fmt.Printf(tick+" %s: added `%s` to Caddyfile\n", hostName, importLine)
		problems--
	}
	return problems
}

const (
	giBlockStart = "# >>> sd managed >>>"
	giBlockEnd   = "# <<< sd managed <<<"
)

// writeManagedBlock writes rules into path inside a marked sd block, creating
// the file if absent and preserving any content outside the markers. Idempotent.
func writeManagedBlock(path string, rules []string) error {
	var existing string
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	block := giBlockStart + "\n" +
		"# sd-generated config under data/ dirs the repo otherwise ignores.\n" +
		"# Managed by 'splitdns doctor --fix'; edit outside these markers.\n" +
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
// re-includes splitdns's generated files when a broad rule like **/data/** would
// otherwise ignore them. Git won't re-include a file under an excluded
// directory, so the directories must be un-ignored first (lines 1–2); then
// only splitdns's file types are re-included (lines 3–4) — runtime data (.db,
// caches, certs, …) stays ignored. Host-agnostic; one block at the repo root.
func unignoreRules() []string {
	return []string{
		"!**/data/",
		"!**/data/**/",
		"!**/data/**/*.generated.conf",
		"!**/data/**/*.caddy",
	}
}
