package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"sd/internal/config"
	"sd/internal/plan"
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
// stay visible — and points at `sd doctor` for the full file list + fix.
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
		warn+" %d generated %s %s gitignored and won't deploy. Run 'sd doctor --fix'.\n",
		len(ignored), noun, verb)
}

// printIgnoreDetail prints the full report: the ignored paths and the per-host
// .gitignore negation lines to add. Used by `sd doctor`.
func printIgnoreDetail(ignored []string) {
	fmt.Printf("%d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\nAdd to .gitignore to un-ignore them (or run 'sd doctor --fix'):")
	for _, rule := range unignoreRules() {
		fmt.Printf("  %s\n", rule)
	}
}

// cmdDoctor audits the repo for problems sd can detect. Read-only by default;
// `sd doctor --fix` applies the .gitignore negations (the one fix sd can make
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
			fmt.Println("\nRun 'sd doctor --fix' to add these entries automatically.")
		}
	}

	// --- Caddy import check ---
	problems += checkCaddyImports(cfg, p)

	if problems > 0 {
		return 1
	}
	return 0
}

// checkCaddyImports runs `caddy adapt` inside the caddy container and reports
// any service FQDNs that don't appear in the adapted config — meaning the
// Caddyfile is missing the required import lines. Skips silently if docker is
// unavailable or the caddy container isn't running.
func checkCaddyImports(cfg *config.Config, p *plan.Plan) int {
	if _, err := exec.LookPath("docker"); err != nil {
		return 0
	}
	const cf = "/etc/caddy/Caddyfile"
	adapted := dexecShAll(caddyContainer, "caddy adapt --config "+cf+" --adapter caddyfile 2>/dev/null")
	if adapted == "" {
		fmt.Println("Skipped Caddy import check (caddy container not reachable).")
		return 0
	}

	adaptedLow := strings.ToLower(adapted)
	problems := 0
	for _, k := range p.Valid() {
		if plan.IsDomainOwner(k) {
			continue
		}
		svc, ok := cfg.Services[k]
		if !ok {
			continue
		}
		if strings.Contains(adaptedLow, strings.ToLower(svc.FQDN)) {
			fmt.Printf(tick+" %s in adapted Caddy config\n", svc.FQDN)
		} else {
			fmt.Printf(cross+" %s missing from adapted Caddy config — Caddyfile must contain 'import tls/*.caddy' then 'import sites/*.caddy'\n", svc.FQDN)
			problems++
		}
	}
	if problems == 0 && len(p.Valid()) > 0 {
		fmt.Println(tick + " All services present in adapted Caddy config.")
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
		"# Managed by 'sd doctor --fix'; edit outside these markers.\n" +
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
// re-includes sd's generated files when a broad rule like **/data/** would
// otherwise ignore them. Git won't re-include a file under an excluded
// directory, so the directories must be un-ignored first (lines 1–2); then
// only sd's file types are re-included (lines 3–4) — runtime data (.db,
// caches, certs, …) stays ignored. Host-agnostic; one block at the repo root.
func unignoreRules() []string {
	return []string{
		"!**/data/",
		"!**/data/**/",
		"!**/data/**/*.generated.conf",
		"!**/data/**/*.caddy",
	}
}
