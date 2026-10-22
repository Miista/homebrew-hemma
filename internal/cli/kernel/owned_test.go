package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, manifestName), []byte("web:\n  - pi/sites/web.caddy\ndns:\n  - pi/dnsmasq.d/web.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// HEAD's manifest still lists a file sync has since removed: its deletion is hemma's change too.
	head := func(p string) ([]byte, bool) {
		if p == manifestName {
			return []byte("gone:\n  - pi/sites/gone.caddy\n"), true
		}
		return nil, false
	}
	owned := OwnedPaths(root, head)
	for _, p := range []string{"services.yaml", manifestName, ".gitignore", "sd-manifest.yaml", "pi/sites/web.caddy", "pi/dnsmasq.d/web.conf", "pi/sites/gone.caddy"} {
		if !owned[p] {
			t.Errorf("%s should be owned", p)
		}
	}
	for _, p := range []string{"pi/docker-compose.yml", "pi/restic/coverage-ignore", "README.md"} {
		if owned[p] {
			t.Errorf("%s belongs to somebody else and must not be owned", p)
		}
	}
	if o := OwnedPaths(t.TempDir(), nil); !o["services.yaml"] || len(o) != 5 {
		t.Errorf("with no manifest anywhere only the fixed files are owned, got %v", o)
	}
}
