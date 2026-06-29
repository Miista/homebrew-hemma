// Package sync is the reconcile engine — the ONLY code that writes or deletes
// generated files (design §6 single-writer invariant). It diffs the desired
// plan against the manifest, writes valid outputs, and (in complete mode)
// GCs orphaned tracked files.
package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"shd/internal/config"
	"shd/internal/manifest"
	"shd/internal/plan"
)

// Mode selects reconcile behavior.
type Mode int

const (
	// Incremental writes/updates valid entries and never deletes (default).
	Incremental Mode = iota
	// Complete is incremental plus GC of orphaned manifest-tracked files.
	Complete
)

// Result summarizes a sync run.
type Result struct {
	Created   []string // paths that did not exist before
	Updated   []string // paths whose content changed
	Deleted   []string // paths removed
	Unchanged []string // paths rewritten with identical content
	Synced    []string // service names successfully synced
	Skipped   map[string]string
	Total     int
}

// Changed reports whether the run altered any file on disk.
func (r *Result) Changed() bool {
	return len(r.Created) > 0 || len(r.Updated) > 0 || len(r.Deleted) > 0
}

// Engine reconciles the repo at repoRoot.
type Engine struct {
	RepoRoot string
	Manifest *manifest.Manifest
}

// Reconcile makes generated files match the plan. It mutates and saves the
// manifest. repoRoot-relative paths in the plan are joined onto RepoRoot for
// disk I/O; the manifest stores repo-relative paths.
func (e *Engine) Reconcile(p *plan.Plan, mode Mode) (*Result, error) {
	res := &Result{Skipped: p.Skipped, Total: p.Total}

	// Snapshot every path the manifest tracked BEFORE this run mutates it. This
	// is the only record of files we previously wrote — including those whose
	// backing host/domain/service is now gone from the YAML entirely, and so
	// can't be re-derived from the plan. GC diffs this against what we write.
	oldTracked := map[string]bool{}
	for _, owner := range e.Manifest.Services() {
		for _, rel := range e.Manifest.Files(owner) {
			oldTracked[rel] = true
		}
	}

	// Write valid entries; collect the set of paths now desired and the owners
	// still present in the plan.
	written := map[string]bool{}
	liveOwner := map[string]bool{}
	for _, svc := range p.Valid() {
		liveOwner[svc] = true
		var paths []string
		for _, f := range p.Files[svc] {
			abs := filepath.Join(e.RepoRoot, f.Path)
			// Classify the write by comparing to what's already on disk, so
			// callers can show only what actually changed.
			switch existing, err := os.ReadFile(abs); {
			case err != nil: // missing (or unreadable) → treat as create
				res.Created = append(res.Created, f.Path)
			case string(existing) == f.Content:
				res.Unchanged = append(res.Unchanged, f.Path)
			default:
				res.Updated = append(res.Updated, f.Path)
			}
			if err := config.AtomicWrite(abs, []byte(f.Content)); err != nil {
				return res, fmt.Errorf("write %s for %s: %w", f.Path, svc, err)
			}
			written[f.Path] = true
			paths = append(paths, f.Path)
		}
		e.Manifest.Set(svc, paths)
		// Synthetic per-domain TLS owners are written and tracked like anything
		// else, but they aren't services — keep them out of the service count.
		if !plan.IsDomainOwner(svc) {
			res.Synced = append(res.Synced, svc)
		}
	}

	if mode == Complete {
		// GC = previously-tracked paths no longer desired. Catches whole-owner
		// removal AND per-owner shrinkage (e.g. a host removed from a surviving
		// domain's cross-product). Only manifest-tracked files are ever touched.
		var stale []string
		for rel := range oldTracked {
			if !written[rel] {
				stale = append(stale, rel)
			}
		}
		for _, rel := range deletionOrder(stale) {
			abs := filepath.Join(e.RepoRoot, rel)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return res, fmt.Errorf("delete %s: %w", rel, err)
			}
			res.Deleted = append(res.Deleted, rel)
		}
		// Drop owners no longer in the plan (their backing service/domain is
		// gone), so the manifest doesn't retain dead entries.
		for _, owner := range e.Manifest.Services() {
			if !liveOwner[owner] {
				e.Manifest.Remove(owner)
			}
		}
	}

	sort.Strings(res.Unchanged)
	sort.Strings(res.Created)
	sort.Strings(res.Updated)
	sort.Strings(res.Deleted)
	sort.Strings(res.Synced)

	if err := e.Manifest.Save(); err != nil {
		return res, fmt.Errorf("save manifest: %w", err)
	}
	return res, nil
}

// deleteService removes a service's manifest-tracked files and drops it from
// the manifest. Shared by --complete GC and the remove command (§6). TLS
// snippets are owned by synthetic @domain: keys, not services, so a service's
// files never overlap another owner's — no cross-reference guard is needed.
//
// Deletion never needs dns_host: paths come from the manifest, not config.
func (e *Engine) deleteService(svc string, res *Result) error {
	for _, rel := range deletionOrder(e.Manifest.Files(svc)) {
		abs := filepath.Join(e.RepoRoot, rel)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete %s for %s: %w", rel, svc, err)
		}
		res.Deleted = append(res.Deleted, rel)
	}
	e.Manifest.Remove(svc)
	return nil
}

// deletionOrder sorts paths so DNS records (dnsmasq .conf) are removed BEFORE
// Caddy blocks. If deletion is interrupted partway, the safe residual state is
// "the name no longer resolves" rather than "it resolves to a host that no
// longer routes it" (a dangling record pointing at a dead backend).
func deletionOrder(paths []string) []string {
	out := append([]string(nil), paths...)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := isDNSRecord(out[i]), isDNSRecord(out[j])
		if di != dj {
			return di // DNS records first
		}
		return out[i] < out[j]
	})
	return out
}

func isDNSRecord(rel string) bool {
	return strings.HasSuffix(rel, ".conf") && strings.Contains(rel, "dnsmasq")
}

// RemoveService deletes one service's tracked files and persists the manifest.
// Used by the remove command (the shared delete primitive, §6).
func (e *Engine) RemoveService(svc string) (*Result, error) {
	res := &Result{Skipped: map[string]string{}}
	if err := e.deleteService(svc, res); err != nil {
		return res, err
	}
	sort.Strings(res.Deleted)
	if err := e.Manifest.Save(); err != nil {
		return res, fmt.Errorf("save manifest: %w", err)
	}
	return res, nil
}
