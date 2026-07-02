package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"mirage/internal/plan"
)

func TestLoad_Missing(t *testing.T) {
	m, ok := Load(filepath.Join(t.TempDir(), "m.yaml"))
	if !ok {
		t.Error("missing manifest should be ok (rebuildable, not corrupt)")
	}
	if len(m.Entries) != 0 {
		t.Errorf("missing manifest should be empty, got %v", m.Entries)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	m, _ := Load(path)
	m.Set("docs", []string{"appbox/x.caddy", "resolver/x.conf"})
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	m2, ok := Load(path)
	if !ok {
		t.Fatal("reload not ok")
	}
	got := m2.Files("docs")
	if len(got) != 2 || got[0] != "appbox/x.caddy" {
		t.Errorf("round-trip mismatch: %v", got)
	}
}

func TestLoad_Unparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("\t- : not : valid\n::"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok := Load(path)
	if ok {
		t.Error("unparseable manifest should report ok=false so caller rebuilds")
	}
}

func TestRebuild_RecordsOnlyExistingFiles(t *testing.T) {
	root := t.TempDir()
	// Create one of the two expected files on disk.
	present := filepath.Join(root, "resolver", "x.conf")
	if err := os.MkdirAll(filepath.Dir(present), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(present, []byte("x"), 0o644)

	p := &plan.Plan{Files: map[string][]plan.File{
		"docs": {
			{Path: "resolver/x.conf"}, // exists
			{Path: "appbox/x.caddy"},  // missing
		},
	}}
	m := Rebuild(filepath.Join(root, "m.yaml"), root, p)
	got := m.Files("docs")
	if len(got) != 1 || got[0] != "resolver/x.conf" {
		t.Errorf("rebuild should record only existing files, got %v", got)
	}
}
