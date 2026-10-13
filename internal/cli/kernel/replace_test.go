package kernel

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func replaceEnv() (*Env, *bytes.Buffer) {
	var out bytes.Buffer
	return &Env{Out: &out, Err: &out, NoTerminal: true}, &out
}

// The recovery path: text that does not parse is refused and the file is untouched.
func TestReplaceConfig_RefusesTextThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	cfg := filepath.Join(dir, configName)
	before, _ := os.ReadFile(cfg)
	env, out := replaceEnv()
	if code := ReplaceConfig(env, dir, cfg, "hosts: [unclosed"); code == 0 {
		t.Fatal("unparseable text must be refused")
	}
	if !strings.Contains(out.String(), "parse") {
		t.Errorf("the reason should say it did not parse: %q", out.String())
	}
	if after, _ := os.ReadFile(cfg); !bytes.Equal(before, after) {
		t.Error("services.yaml changed although the text was refused")
	}
}

// ...and it can rescue a file the loader rejects: edit it into shape, save, everything works again.
func TestReplaceConfig_RescuesAnUnloadableFile(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	cfg := filepath.Join(dir, configName)
	// An auth mapping with no mode at all: the loader refuses the WHOLE file for it.
	broken := "hosts:\n  resolver: {ip: 192.0.2.1}\n  appbox: {ip: 192.0.2.2}\ndomains:\n  - example.com\ndefaults:\n  dns_host: resolver\nservices:\n  web:\n    fqdn: web.example.com\n    host: appbox\n    backend: web:80\n    auth:\n      groups: [admins]\n"
	if err := os.WriteFile(cfg, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	env, out := replaceEnv()
	if code := Route2(env, dir, cfg, "list", nil); code == 0 {
		t.Fatalf("precondition: the broken file should not load\n%s", out.String())
	}

	fixed := strings.Replace(broken, "    auth:\n      groups: [admins]\n", "    auth:\n      mode: forward\n      groups: [admins]\n", 1)
	env, out = replaceEnv()
	if code := ReplaceConfig(env, dir, cfg, fixed); code > 1 {
		t.Fatalf("replace: %d\n%s", code, out.String())
	}
	if got, _ := os.ReadFile(cfg); string(got) != fixed {
		t.Errorf("the text must be written exactly as given:\n%s", got)
	}
	env, out = replaceEnv()
	if code := Route2(env, dir, cfg, "list", []string{"--all"}); code != 0 || !strings.Contains(out.String(), "web") {
		t.Errorf("after the rescue the file must load: %d\n%s", code, out.String())
	}
}

// Route2 runs a kernel command by word (test helper).
func Route2(env *Env, repoRoot, cfgPath, cmd string, args []string) int {
	code, _ := Route(env, repoRoot, cfgPath, cmd, args)
	return code
}
