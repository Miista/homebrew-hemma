package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

func TestCheckCaddyfileImports_RewritesLegacyLine(t *testing.T) {
	// Both pre-rename generations must be rewritten in place: the sd-era line
	// and the splitdns-era line.
	for _, legacy := range []string{"sd.generated.caddy", "splitdns.generated.caddy"} {
		t.Run(legacy, func(t *testing.T) {
			root := t.TempDir()
			hostDir := filepath.Join(root, "web", config.DefaultCaddyDataDir)
			if err := os.MkdirAll(hostDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cf := filepath.Join(hostDir, "Caddyfile")
			if err := os.WriteFile(cf, []byte("# my caddy\nimport "+legacy+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Hosts: map[string]config.Host{"web": {IP: "192.0.2.1"}}}

			if problems := checkCaddyfileImports(root, cfg, true); problems != 0 {
				t.Errorf("problems = %d, want 0", problems)
			}
			got, _ := os.ReadFile(cf)
			if strings.Contains(string(got), legacy) || !strings.Contains(string(got), "hemma.generated.caddy") {
				t.Errorf("legacy import not rewritten:\n%s", got)
			}
			if strings.Count(string(got), "import ") != 1 {
				t.Errorf("expected exactly one import line, got:\n%s", got)
			}
		})
	}
}

func TestWriteManagedBlock_MigratesLegacyMarkers(t *testing.T) {
	// Both pre-rename marker generations (sd, splitdns) must be replaced in
	// place, not left behind next to a duplicate hemma block.
	for _, era := range []string{"sd", "splitdns"} {
		t.Run(era, func(t *testing.T) {
			dir := t.TempDir()
			gi := filepath.Join(dir, ".gitignore")
			legacy := "node_modules/\n\n# >>> " + era + " managed >>>\n# old\n!old-rule\n# <<< " + era + " managed <<<\n"
			if err := os.WriteFile(gi, []byte(legacy), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := writeManagedBlock(gi, []string{"!new-rule"}); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(gi)
			s := string(got)
			if strings.Contains(s, era+" managed") {
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
		})
	}
}
