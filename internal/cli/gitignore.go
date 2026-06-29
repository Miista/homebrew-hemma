package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"shd/internal/plan"
)

// warnIfIgnored checks every path the plan would write and, if any are
// gitignored, prints a warning to stderr with the per-host .gitignore lines to
// add. shd does not edit .gitignore itself — that's the user's file. Silent
// when nothing is ignored or the check can't run (git absent / not a repo).
func warnIfIgnored(repoRoot string, p *plan.Plan) {
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
	ignored, ok := ignoredPaths(repoRoot, paths)
	if !ok || len(ignored) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "Warning: %d generated %s ignored by git — they won't be committed or deployed:\n",
		len(ignored), plural(len(ignored), "file"))
	for _, p := range ignored {
		fmt.Fprintf(os.Stderr, "  %s\n", p)
	}
	fmt.Fprintln(os.Stderr, "\nAdd these lines to un-ignore them (shd won't edit .gitignore for you):")
	sugg := unignoreSuggestions(ignored)
	for _, host := range sortedKeysOf(sugg) {
		fmt.Fprintf(os.Stderr, "  # in %s/.gitignore\n", host)
		for _, rule := range sugg[host] {
			fmt.Fprintf(os.Stderr, "  %s\n", rule)
		}
	}
}

// cmdDoctor audits the repo for problems shd can detect but doesn't fix
// automatically. Currently: generated files that git would ignore (so they'd
// never commit/deploy). Read-only. Exits non-zero if it finds problems.
func cmdDoctor(cfgPath string, args []string) int {
	repoRoot := filepath.Dir(cfgPath)
	cfg, code := loadExisting(cfgPath, "check")
	if cfg == nil {
		return code
	}
	p := plan.Build(cfg)

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
	ignored, ok := ignoredPaths(repoRoot, paths)
	if !ok {
		fmt.Println("Skipped gitignore check (git not available or not a repository).")
		return 0
	}
	if len(ignored) == 0 {
		fmt.Println("✓ No generated files are gitignored.")
		return 0
	}
	warnIfIgnored(repoRoot, p)
	return 1
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
