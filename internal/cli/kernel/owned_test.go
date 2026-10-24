package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, manifestName), []byte("web:\n  - edge/sites/web.caddy\ndns:\n  - edge/dnsmasq.d/web.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// HEAD's manifest still lists a file sync has since removed: its deletion is hemma's change too.
	head := func(p string) ([]byte, bool) {
		if p == manifestName {
			return []byte("gone:\n  - edge/sites/gone.caddy\n"), true
		}
		return nil, false
	}
	owned := OwnedPaths(root, head)
	for _, p := range []string{"services.yaml", manifestName, "sd-manifest.yaml", "edge/sites/web.caddy", "edge/dnsmasq.d/web.conf", "edge/sites/gone.caddy"} {
		if !owned[p] {
			t.Errorf("%s should be owned", p)
		}
	}
	for _, p := range []string{".gitignore", "edge/docker-compose.yml", "edge/restic/coverage-ignore", "README.md"} {
		if owned[p] {
			t.Errorf("%s belongs to somebody else and must not be owned", p)
		}
	}
	if o := OwnedPaths(t.TempDir(), nil); !o["services.yaml"] || len(o) != 4 {
		t.Errorf("with no manifest anywhere only the fixed files are owned, got %v", o)
	}
}

// .gitignore is shared with the person: only hemma's marked block is hemma's. The file is owned while that block
// differs from the committed one, and not for an edit outside it.
func TestOwnedPaths_GitignoreOnlyForHemmaBlock(t *testing.T) {
	root := t.TempDir()
	block := giBlockStart + "\n!edge/sites/\n" + giBlockEnd + "\n"
	committed := "# mine\ndata/\n\n" + block
	head := func(p string) ([]byte, bool) {
		if p == ".gitignore" {
			return []byte(committed), true
		}
		return nil, false
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(committed)
	if OwnedPaths(root, head)[".gitignore"] {
		t.Error("an unchanged .gitignore is nobody's change")
	}
	write("# mine\ndata/\nbackups/\n\n" + block) // a hand edit outside the block
	if OwnedPaths(root, head)[".gitignore"] {
		t.Error("an edit outside hemma's block is the person's, not hemma's")
	}
	write("# mine\ndata/\n\n" + giBlockStart + "\n!edge/sites/\n!edge/dnsmasq.d/\n" + giBlockEnd + "\n") // doctor --fix grew the block
	if !OwnedPaths(root, head)[".gitignore"] {
		t.Error("a changed hemma block is hemma's change")
	}
	write("# mine\ndata/\n\n" + "# >>> splitdns managed >>>\n!edge/sites/\n# <<< splitdns managed <<<\n") // only the markers were migrated
	if OwnedPaths(root, head)[".gitignore"] {
		t.Error("pre-rename markers read as the current ones: same block, not a change")
	}
	write("# mine\ndata/\n") // the block was removed
	if !OwnedPaths(root, head)[".gitignore"] {
		t.Error("a removed hemma block is hemma's change")
	}
}
