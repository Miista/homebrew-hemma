package kernel

import (
	"os"
	"path/filepath"
	"strings"

	"hemma/internal/manifest"
)

// OwnedPaths is the set of repo paths hemma is responsible for: services.yaml, the manifest, every file the manifest
// says hemma generated, and .gitignore while the block `doctor --fix` writes in it differs from the committed one.
// Anything else in the repo (a compose file, a backup list, a note, a hand edit elsewhere in .gitignore) is somebody
// else's work — hemma neither shows it as its own change, nor commits it, nor lets it block a deploy.
//
// The manifest is the authority, the same one that decides what hemma may delete. Both the working copy and the
// committed one (headFile reads a path as of HEAD) count: a file sync just removed is already gone from the new
// manifest, but its deletion is still hemma's change to commit. headFile may be nil.
func OwnedPaths(repoRoot string, headFile func(path string) ([]byte, bool)) map[string]bool {
	names := append([]string{manifestName}, legacyManifestNames...)
	owned := map[string]bool{configName: true}
	for _, n := range names {
		owned[n] = true
	}
	// .gitignore is shared: hemma writes only its marked block (doctor --fix), the rest is the person's. So the file
	// counts as hemma's change only while that block differs from the committed one; a hand edit outside it is theirs.
	var wc, head []byte
	wc, _ = os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if headFile != nil {
		head, _ = headFile(".gitignore")
	}
	if managedBlock(string(wc)) != managedBlock(string(head)) {
		owned[".gitignore"] = true
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

// managedBlock is the text of hemma's marked block in a .gitignore (pre-rename markers read as the current ones),
// or "" when there is none.
func managedBlock(content string) string {
	for _, m := range legacyGiBlockStarts {
		content = strings.ReplaceAll(content, m, giBlockStart)
	}
	for _, m := range legacyGiBlockEnds {
		content = strings.ReplaceAll(content, m, giBlockEnd)
	}
	s, e := strings.Index(content, giBlockStart), strings.Index(content, giBlockEnd)
	if s < 0 || e <= s {
		return ""
	}
	return content[s : e+len(giBlockEnd)]
}
