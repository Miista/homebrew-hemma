package kernel

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/config"
	"hemma/internal/manifest"
	"hemma/internal/render"
)

// OwnedPaths is the set of repo paths hemma is responsible for: services.yaml, the manifest, every file the manifest
// says hemma generated, and — while the one part of them hemma writes differs from the committed one — .gitignore
// (its managed block) and each host's Caddyfile (its import lines, see doctor --fix). Anything else in the repo (a
// compose file, a backup list, a note, a hand edit elsewhere in .gitignore or the Caddyfile) is somebody else's
// work — hemma neither shows it as its own change, nor commits it, nor lets it block a deploy.
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
	// Each host's Caddyfile is the same kind of file: hand-maintained, and hemma writes exactly one thing in it — the
	// import lines for its generated snippets (doctor --fix adds or renames them). So it is hemma's change only while
	// those lines differ from the committed ones. Which Caddyfiles there are comes from services.yaml; when that cannot
	// be read, none are considered.
	for _, cf := range caddyfilePaths(repoRoot) {
		wc, _ := os.ReadFile(filepath.Join(repoRoot, cf))
		var head []byte
		if headFile != nil {
			head, _ = headFile(cf)
		}
		if hemmaImports(string(wc)) != hemmaImports(string(head)) {
			owned[cf] = true
		}
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

// caddyfilePaths lists every host's Caddyfile, repo-relative, as services.yaml declares the hosts (the same place
// doctor looks). An unreadable services.yaml yields none.
func caddyfilePaths(repoRoot string) []string {
	data, err := os.ReadFile(filepath.Join(repoRoot, configName))
	if err != nil {
		return nil
	}
	cfg, err := config.Parse(data, filepath.Join(repoRoot, configName))
	if err != nil {
		return nil
	}
	var paths []string
	for name, h := range cfg.Hosts {
		paths = append(paths, filepath.ToSlash(filepath.Join(h.ResolvedDir(name), config.DefaultCaddyDataDir, "Caddyfile")))
	}
	sort.Strings(paths)
	return paths
}

// hemmaImportLines are the lines hemma puts in a Caddyfile, current and pre-rename: the import of the generated
// site blocks and of the trusted-proxies snippet.
var hemmaImportLines = []string{
	"import " + render.CaddyImportFilename,
	"import " + render.TrustedProxiesFilename,
	"import splitdns.generated.caddy",
	"import sd.generated.caddy",
}

// hemmaImports is the set of hemma's import lines present in a Caddyfile, as one comparable string.
func hemmaImports(content string) string {
	var present []string
	for _, l := range hemmaImportLines {
		if strings.Contains(content, l) {
			present = append(present, l)
		}
	}
	return strings.Join(present, "\n")
}

// managedBlock is the text of hemma's marked block in a .gitignore, markers included and as written — so a block
// whose pre-rename markers doctor --fix rewrote to the current ones is a change too, not an invisible diff — or ""
// when there is none.
func managedBlock(content string) string {
	starts := append([]string{giBlockStart}, legacyGiBlockStarts...)
	ends := append([]string{giBlockEnd}, legacyGiBlockEnds...)
	for i := range starts {
		s, e := strings.Index(content, starts[i]), strings.Index(content, ends[i])
		if s >= 0 && e > s {
			return content[s : e+len(ends[i])]
		}
	}
	return ""
}

// SharedPath reports whether a repo path is one hemma shares with the person: hemma writes one part of it (see
// OwnedPaths: .gitignore's managed block, a Caddyfile's import lines) and the rest is theirs. Such a file may be
// hemma's change to commit, but never hemma's to discard or remove whole.
func SharedPath(p string) bool {
	return p == ".gitignore" || filepath.Base(p) == "Caddyfile"
}
