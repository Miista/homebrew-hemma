package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"splitdns/internal/config"
)

func TestCheckCaddyfileImports_RewritesLegacyLine(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, "web", config.DefaultCaddyDataDir)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join(hostDir, "Caddyfile")
	if err := os.WriteFile(cf, []byte("# my caddy\nimport sd.generated.caddy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Hosts: map[string]config.Host{"web": {IP: "192.0.2.1"}}}

	if problems := checkCaddyfileImports(root, cfg, true); problems != 0 {
		t.Errorf("problems = %d, want 0", problems)
	}
	got, _ := os.ReadFile(cf)
	if strings.Contains(string(got), "sd.generated.caddy") && !strings.Contains(string(got), "splitdns.generated.caddy") {
		t.Errorf("legacy import not rewritten:\n%s", got)
	}
	if strings.Count(string(got), "import ") != 1 {
		t.Errorf("expected exactly one import line, got:\n%s", got)
	}
}

func TestWriteManagedBlock_MigratesLegacyMarkers(t *testing.T) {
	dir := t.TempDir()
	gi := filepath.Join(dir, ".gitignore")
	legacy := "node_modules/\n\n# >>> sd managed >>>\n# old\n!old-rule\n# <<< sd managed <<<\n"
	if err := os.WriteFile(gi, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedBlock(gi, []string{"!new-rule"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(gi)
	s := string(got)
	if strings.Contains(s, "sd managed") {
		t.Errorf("legacy markers survived:\n%s", s)
	}
	if strings.Count(s, giBlockStart) != 1 || strings.Count(s, giBlockEnd) != 1 {
		t.Errorf("expected exactly one managed block:\n%s", s)
	}
	if !strings.Contains(s, "!new-rule") || strings.Contains(s, "!old-rule") {
		t.Errorf("block not replaced:\n%s", s)
	}
	if !strings.Contains(s, "node_modules/") {
		t.Errorf("content outside block not preserved:\n%s", s)
	}
}
