package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadManifest_MigratesLegacyNames verifies both pre-rename manifest
// filenames (sd era, splitdns era) are renamed to hemma-manifest.yaml on
// first load, preserving their tracked-file history (the GC authority).
func TestLoadManifest_MigratesLegacyNames(t *testing.T) {
	for _, legacy := range []string{"sd-manifest.yaml", "splitdns-manifest.yaml"} {
		t.Run(legacy, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir)
			content := "docs:\n  - appbox/caddy/data/sites/docs.caddy\n"
			if err := os.WriteFile(filepath.Join(dir, legacy), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := load(t, dir)
			mf := loadManifest(dir, cfg)
			if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
				t.Errorf("%s not created: %v", manifestName, err)
			}
			if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
				t.Errorf("legacy %s still present", legacy)
			}
			if got := mf.Files("docs"); len(got) != 1 || got[0] != "appbox/caddy/data/sites/docs.caddy" {
				t.Errorf("tracked-file history lost across migration: %v", got)
			}
		})
	}
}

// TestDoctorFix_MigratesSplitdnsArtifacts is the end-to-end rename migration:
// a repo with splitdns-era artifacts (old manifest name, old generated
// filenames, old Caddyfile import line) must come out of `doctor --fix` with
// hemma-named artifacts, a rewritten import line, and the old generated files
// GC'd via the migrated manifest.
func TestDoctorFix_MigratesSplitdnsArtifacts(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 0 {
		t.Fatalf("add failed: %d", code)
	}

	// Rewind the repo to the splitdns era: rename the generated per-host caddy
	// files and the manifest to their old names, and point the Caddyfile and
	// manifest entries at them.
	oldNew := [][2]string{
		{"appbox/caddy/data/splitdns.generated.caddy", "appbox/caddy/data/hemma.generated.caddy"},
		{"appbox/caddy/data/splitdns.auth.generated.caddy", "appbox/caddy/data/hemma.auth.generated.caddy"},
		{"resolver/caddy/data/splitdns.generated.caddy", "resolver/caddy/data/hemma.generated.caddy"},
		{"resolver/caddy/data/splitdns.auth.generated.caddy", "resolver/caddy/data/hemma.auth.generated.caddy"},
	}
	for _, p := range oldNew {
		if err := os.Rename(filepath.Join(dir, p[1]), filepath.Join(dir, p[0])); err != nil {
			t.Fatal(err)
		}
	}
	mfBytes, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	oldMf := strings.ReplaceAll(string(mfBytes), "hemma.", "splitdns.")
	if err := os.WriteFile(filepath.Join(dir, "splitdns-manifest.yaml"), []byte(oldMf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, manifestName)); err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join(dir, "appbox/caddy/data/Caddyfile")
	if err := os.WriteFile(cf, []byte("import splitdns.generated.caddy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := Run([]string{"-C", dir, "doctor", "--fix"}); code != 0 {
		t.Fatalf("doctor --fix exit = %d, want 0", code)
	}

	// Manifest migrated.
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		t.Errorf("hemma manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "splitdns-manifest.yaml")); !os.IsNotExist(err) {
		t.Error("splitdns-manifest.yaml still present")
	}
	// Old artifacts GC'd, new ones written.
	for _, p := range oldNew {
		if _, err := os.Stat(filepath.Join(dir, p[0])); !os.IsNotExist(err) {
			t.Errorf("old artifact not GC'd: %s", p[0])
		}
		if _, err := os.Stat(filepath.Join(dir, p[1])); err != nil {
			t.Errorf("new artifact missing: %s", p[1])
		}
	}
	// Caddyfile import line rewritten.
	got, _ := os.ReadFile(cf)
	if strings.Contains(string(got), "splitdns.generated.caddy") || !strings.Contains(string(got), "import hemma.generated.caddy") {
		t.Errorf("Caddyfile import not rewritten:\n%s", got)
	}
}
