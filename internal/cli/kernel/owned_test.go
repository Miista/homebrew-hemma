package kernel

import (
	"os"
	"path/filepath"
	"strings"
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
	write("# mine\ndata/\n\n" + "# >>> splitdns managed >>>\n!edge/sites/\n# <<< splitdns managed <<<\n") // only the markers differ
	if !OwnedPaths(root, head)[".gitignore"] {
		t.Error("markers doctor --fix migrates are part of the block: a rename is hemma's change, not an invisible diff")
	}
	write("# mine\ndata/\n") // the block was removed
	if !OwnedPaths(root, head)[".gitignore"] {
		t.Error("a removed hemma block is hemma's change")
	}
}

// A host's Caddyfile is hand-maintained; hemma writes only its import lines there (doctor --fix). The file is owned
// while those lines differ from the committed ones, and not for an edit elsewhere in it.
func TestOwnedPaths_CaddyfileOnlyForHemmaImports(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "services.yaml"), []byte("hosts:\n  edge: {ip: 192.0.2.1}\n  nas: {ip: 192.0.2.2, dir: storage}\nservices: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join("edge", "caddy", "data", "Caddyfile")
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(cf)), 0o755); err != nil {
		t.Fatal(err)
	}
	committed := "{\n  servers {\n    import hemma.trusted_proxies.generated.caddy\n  }\n}\nimport hemma.generated.caddy\n"
	head := func(p string) ([]byte, bool) {
		if p == "edge/caddy/data/Caddyfile" {
			return []byte(committed), true
		}
		return nil, false
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, cf), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(committed)
	if o := OwnedPaths(root, head); o["edge/caddy/data/Caddyfile"] || o["storage/caddy/data/Caddyfile"] {
		t.Errorf("an unchanged (or absent) Caddyfile is nobody's change: %v", o)
	}
	write(committed + "\nexample.com {\n  respond ok\n}\n") // a hand-written site block
	if OwnedPaths(root, head)["edge/caddy/data/Caddyfile"] {
		t.Error("an edit outside hemma's import lines is the person's")
	}
	write("{\n  servers {\n  }\n}\nimport hemma.generated.caddy\n") // the trusted-proxies import is gone
	if !OwnedPaths(root, head)["edge/caddy/data/Caddyfile"] {
		t.Error("a missing hemma import is hemma's change (doctor --fix would add it back)")
	}
	write(strings.ReplaceAll(committed, "import hemma.generated.caddy", "import splitdns.generated.caddy")) // pre-rename import line
	if !OwnedPaths(root, head)["edge/caddy/data/Caddyfile"] {
		t.Error("a renamed import line is hemma's change (doctor --fix rewrites it)")
	}
	// A Caddyfile hemma has never touched and HEAD does not have: no import lines on either side, not owned.
	if err := os.MkdirAll(filepath.Join(root, "storage", "caddy", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "storage", "caddy", "data", "Caddyfile"), []byte("nas.example.com {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if OwnedPaths(root, head)["storage/caddy/data/Caddyfile"] {
		t.Error("a Caddyfile without hemma's lines, new or not, is not hemma's")
	}
}
