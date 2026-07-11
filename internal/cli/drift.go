package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hemma/internal/config"
	"hemma/internal/manifest"
	"hemma/internal/plan"
)

// Drift is the difference between what services.yaml/manifest say the generated
// files should be and what is actually on disk in the repo. It is a pure repo
// concept (no docker) so every command can detect it cheaply.
//
//   - Missing:  a desired generated file is absent on disk.
//   - Modified: a generated file's on-disk content differs from what the plan
//     would write (e.g. hand-edited).
//   - Orphaned: a manifest-tracked file no longer desired by the current plan
//     (e.g. a service/host/domain was removed but its files linger). This is the
//     GC target that only `hemma sync --complete` used to catch.
type Drift struct {
	Missing  []string
	Modified []string
	Orphaned []string
}

// Any reports whether any drift class is non-empty.
func (d Drift) Any() bool {
	return len(d.Missing) > 0 || len(d.Modified) > 0 || len(d.Orphaned) > 0
}

// Count is the total number of drifted files across all classes.
func (d Drift) Count() int {
	return len(d.Missing) + len(d.Modified) + len(d.Orphaned)
}

// detectDrift compares the plan built from cfg (and the manifest) against files
// on disk under repoRoot. Paths returned are repo-relative, sorted.
func detectDrift(repoRoot string, cfg *config.Config, mf *manifest.Manifest) Drift {
	// Populate the auth snippet body so the planned (auth) file's content
	// reflects the live source — this is what makes drift detection catch "the
	// auth_snippet source changed but wasn't synced". If the source is
	// unreadable, pin the planned content to disk so we don't falsely report the
	// last-good file as modified (consistent with runSync's keep-last-good).
	authErr := cfg.LoadAuthSnippet(repoRoot)
	p := plan.Build(cfg)
	if authErr != nil {
		plan.PinAuthSnippetToDisk(p, repoRoot)
	}

	desired := map[string]bool{}
	var d Drift
	for _, svc := range p.Valid() {
		for _, f := range p.Files[svc] {
			desired[f.Path] = true
			abs := filepath.Join(repoRoot, f.Path)
			switch existing, err := os.ReadFile(abs); {
			case err != nil:
				d.Missing = append(d.Missing, f.Path)
			case string(existing) != f.Content:
				d.Modified = append(d.Modified, f.Path)
			}
		}
	}

	// Orphaned: manifest-tracked files no longer desired that still exist on disk.
	for _, owner := range mf.Services() {
		for _, rel := range mf.Files(owner) {
			if desired[rel] {
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
				d.Orphaned = append(d.Orphaned, rel)
			}
		}
	}

	sort.Strings(d.Missing)
	sort.Strings(d.Modified)
	sort.Strings(d.Orphaned)
	return d
}

// reportDrift prints a one-line status when drift exists, followed by the
// affected files. Commands call this to surface drift as part of their own
// output (report-but-proceed). Prints nothing when the repo is clean.
func reportDrift(d Drift) {
	if !d.Any() {
		return
	}
	fmt.Printf("\n%s Repo drift: %d %s out of sync with services.yaml — run 'hemma doctor --fix'.\n",
		warn, d.Count(), plural(d.Count(), "generated file"))
	printDriftDetail(d)
}

// printDriftDetail lists each drifted file grouped by class.
func printDriftDetail(d Drift) {
	for _, f := range d.Missing {
		fmt.Printf("  - missing:  %s\n", f)
	}
	for _, f := range d.Modified {
		fmt.Printf("  - modified: %s\n", f)
	}
	for _, f := range d.Orphaned {
		fmt.Printf("  - orphaned: %s\n", f)
	}
}
