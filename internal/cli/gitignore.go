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
		"Warning: %d generated %s %s gitignored and won't deploy. Run 'shd doctor --fix'.\n",
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
	fmt.Println("\nThese .gitignore entries would un-ignore them (or run 'shd doctor --fix'):")
	sugg := unignoreSuggestions(ignored)
	for _, host := range sortedKeysOf(sugg) {
		fmt.Printf("  # in %s/.gitignore\n", host)
		for _, rule := range sugg[host] {
			fmt.Printf("  %s\n", rule)
		}
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
		fmt.Println("✓ No generated files are gitignored.")
		return 0
	}

	if !fix {
		printIgnoreDetail(ignored)
		fmt.Println("\nRun 'shd doctor --fix' to add these entries automatically.")
		return 1
	}

	// fix: write the negations into each host's .gitignore (marked block),
	// then re-verify.
	sugg := unignoreSuggestions(ignored)
	for _, host := range sortedKeysOf(sugg) {
		gi := filepath.Join(repoRoot, host, ".gitignore")
		if err := writeManagedBlock(gi, sugg[host]); err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Printf("✓ Updated %s/.gitignore\n", host)
	}

	// Re-verify: the negations should now un-ignore the files.
	still, ok := ignoredPaths(repoRoot, planPaths(p))
	if !ok {
		return 0 // can't re-check; assume the write was enough
	}
	if len(still) == 0 {
		fmt.Println("✓ All generated files are now tracked by git.")
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

// unignoreSuggestions turns a set of ignored repo-relative paths into the
// per-host .gitignore negation lines that would re-include them. Git requires
// un-ignoring each parent directory before the files within, so the chain is
// emitted top-down. Keyed by host directory (the dir holding the .gitignore to
// edit), value is the ordered list of "!..."-rules relative to that host dir.
func unignoreSuggestions(ignored []string) map[string][]string {
	out := map[string][]string{}
	for _, p := range ignored {
		host, rel := splitHostRel(p)
		if host == "" {
			continue
		}
		seen := map[string]bool{}
		add := func(rule string) {
			if !seen[rule] {
				seen[rule] = true
				out[host] = append(out[host], rule)
			}
		}
		// Emit each parent dir top-down, then a glob for the leaf dir's contents.
		segs := strings.Split(filepath.ToSlash(rel), "/")
		for i := 1; i < len(segs); i++ {
			add("!" + strings.Join(segs[:i], "/") + "/")
		}
		dir := strings.Join(segs[:len(segs)-1], "/")
		add("!" + dir + "/**")
	}
	for host := range out {
		out[host] = dedupeStable(out[host])
	}
	return out
}

// splitHostRel splits a repo-relative path "host/rest..." into its first
// segment (the host dir) and the remainder.
func splitHostRel(p string) (host, rel string) {
	p = filepath.ToSlash(p)
	if i := strings.IndexByte(p, '/'); i > 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func dedupeStable(in []string) []string {
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
