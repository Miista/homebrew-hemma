// Package manifest loads, saves, and rebuilds the manifest that records which
// files each service generated (design §5). It is the authority for safe
// deletion: only manifest-tracked files are ever deleted.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"sd/internal/config"
	"sd/internal/plan"
)

// Manifest maps service name -> the file paths it generated.
type Manifest struct {
	Entries map[string][]string `yaml:",inline"`
	path    string
}

// Load reads the manifest. An unparseable manifest is NOT fatal (design §7):
// the caller should Rebuild instead. Load signals that via ok=false.
func Load(path string) (m *Manifest, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing manifest -> empty, rebuildable.
		return &Manifest{Entries: map[string][]string{}, path: path}, os.IsNotExist(err)
	}
	var entries map[string][]string
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return &Manifest{Entries: map[string][]string{}, path: path}, false
	}
	if entries == nil {
		entries = map[string][]string{}
	}
	return &Manifest{Entries: entries, path: path}, true
}

// Rebuild re-derives expected filenames from the plan and records those that
// exist on disk, rooted at repoRoot (design §5).
func Rebuild(path, repoRoot string, p *plan.Plan) *Manifest {
	m := &Manifest{Entries: map[string][]string{}, path: path}
	for svc, files := range p.Files {
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(repoRoot, f.Path)); err == nil {
				m.Entries[svc] = append(m.Entries[svc], f.Path)
			}
		}
		sort.Strings(m.Entries[svc])
	}
	return m
}

// Save writes the manifest atomically.
func (m *Manifest) Save() error {
	data, err := yaml.Marshal(m.Entries)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return config.AtomicWrite(m.path, data)
}

// Set records the files for a service, replacing any prior entry.
func (m *Manifest) Set(service string, paths []string) {
	sort.Strings(paths)
	m.Entries[service] = paths
}

// Files returns the tracked files for a service.
func (m *Manifest) Files(service string) []string { return m.Entries[service] }

// Remove drops a service from the manifest.
func (m *Manifest) Remove(service string) { delete(m.Entries, service) }

// Services returns tracked service names (sorted).
func (m *Manifest) Services() []string {
	out := make([]string, 0, len(m.Entries))
	for s := range m.Entries {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
