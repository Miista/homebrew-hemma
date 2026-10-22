package kernel

import (
	"os"
	"path/filepath"

	"hemma/internal/manifest"
)

// OwnedPaths is the set of repo paths hemma is responsible for: services.yaml, the manifest, the .gitignore blocks
// `doctor --fix` writes, and every file the manifest says hemma generated. Anything else in the repo (a compose file, a
// backup list, a note) is somebody else's work — hemma neither shows it as its own change, nor commits it, nor lets it
// block a deploy.
//
// The manifest is the authority, the same one that decides what hemma may delete. Both the working copy and the
// committed one (headFile reads a path as of HEAD) count: a file sync just removed is already gone from the new
// manifest, but its deletion is still hemma's change to commit. headFile may be nil.
func OwnedPaths(repoRoot string, headFile func(path string) ([]byte, bool)) map[string]bool {
	names := append([]string{manifestName}, legacyManifestNames...)
	owned := map[string]bool{configName: true, ".gitignore": true}
	for _, n := range names {
		owned[n] = true
	}
	add := func(data []byte) {
		if m, ok := manifest.Parse(data); ok {
			for _, svc := range m.Services() {
				for _, f := range m.Files(svc) {
					owned[f] = true
				}
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(repoRoot, manifestName)); err == nil {
		add(data)
	}
	if headFile != nil {
		for _, n := range names {
			if data, ok := headFile(n); ok {
				add(data)
			}
		}
	}
	return owned
}
