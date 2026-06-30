package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"shd/internal/config"
	"shd/internal/plan"
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
// stay visible — and points at `shd doctor` for the full file list + fix.
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
		warn+" %d generated %s %s gitignored and won't deploy. Run 'shd doctor --fix'.\n",
		len(ignored), noun, verb)
}

// printIgnoreDetail prints the full report: the ignored paths and the per-host
// .gitignore negation lines to add. Used by `shd doctor`.
func printIgnoreDetail(ignored []string) {
	fmt.Printf("%d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\nAdd to .gitignore to un-ignore them (or run 'shd doctor --fix'):")
	for _, rule := range unignoreRules() {
		fmt.Printf("  %s\n", rule)
	}
}

// cmdDoctor audits the repo for problems shd can detect. Read-only by default;
// `shd doctor --fix` applies the .gitignore negations (the one fix shd can make
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

	ignored, ok := ignoredPaths(repoRoot, planPaths(p))
	if !ok {
		fmt.Println("Skipped gitignore check (git not available or not a repository).")
		return 0
	}
	if len(ignored) == 0 {
		fmt.Println(tick+" No generated files are gitignored.")
		return 0
	}

	if !fix {
		printIgnoreDetail(ignored)
		fmt.Println("\nRun 'shd doctor --fix' to add these entries automatically.")
		return 1
	}

	// fix: write the negation block to the repo-root .gitignore, then re-verify.
	// The globs are repo-wide and file-type-scoped (see unignoreRules), so one
	// block at the root covers every host and never un-ignores runtime data.
	gi := filepath.Join(repoRoot, ".gitignore")
	if err := writeManagedBlock(gi, unignoreRules()); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Println(tick+" Updated .gitignore")

	// Re-verify: the negations should now un-ignore the files.
	still, ok := ignoredPaths(repoRoot, planPaths(p))
	if !ok {
		return 0 // can't re-check; assume the write was enough
	}
	if len(still) == 0 {
		fmt.Println(tick+" All generated files are now tracked by git.")
		return 0
	}
	fmt.Fprintln(os.Stderr)
	errf("%d %s still gitignored after fix:", len(still), plural(len(still), "file"))
	for _, p := range still {
		fmt.Fprintf(os.Stderr, "  %s\n", p)
	}
	return 1
}

const (
	giBlockStart = "# >>> shd managed >>>"
	giBlockEnd   = "# <<< shd managed <<<"
)

// writeManagedBlock writes rules into path inside a marked shd block, creating
// the file if absent and preserving any content outside the markers. Idempotent.
func writeManagedBlock(path string, rules []string) error {
	var existing string
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	block := giBlockStart + "\n" +
		"# shd-generated config under data/ dirs the repo otherwise ignores.\n" +
		"# Managed by 'shd doctor --fix'; edit outside these markers.\n" +
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
// re-includes shd's generated files when a broad rule like **/data/** would
// otherwise ignore them. Git won't re-include a file under an excluded
// directory, so the directories must be un-ignored first (lines 1–2); then
// only shd's file types are re-included (lines 3–4) — runtime data (.db,
// caches, certs, …) stays ignored. Host-agnostic; one block at the repo root.
func unignoreRules() []string {
	return []string{
		"!**/data/",
		"!**/data/**/",
		"!**/data/**/*.generated.conf",
		"!**/data/**/*.caddy",
	}
}
