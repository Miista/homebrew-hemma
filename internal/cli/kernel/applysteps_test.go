package kernel

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"hemma/internal/config"
)

// ApplySteps is what a front end tells the user before running apply/deploy. It
// must say exactly what apply then does — for every combination of which host
// this machine is, where the auth service runs, and where the tunnel service
// runs — or the dialog would promise a restart that never happens (or hide one
// that does).
func TestApplySteps_MatchesWhatApplyActuallyDoes(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_LOG", logPath)

	hosts := []string{"resolver", "appbox"}
	for _, self := range hosts {
		for _, authHost := range []string{"", "resolver", "appbox"} {
			for _, tunnelHost := range []string{"", "resolver", "appbox"} {
				name := fmt.Sprintf("self=%s/auth=%s/tunnel=%s", self, authHost, tunnelHost)
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					mkdirs(t, dir, "resolver", "appbox")
					yaml := "hosts:\n  resolver: {ip: 192.0.2.1}\n  appbox: {ip: 192.0.2.2}\ndomains:\n  - example.com\ndefaults:\n  dns_host: resolver\n  tunnel_dir: cloudflared\n"
					svc := func(n, host string, extra string) string {
						return fmt.Sprintf("  %s:\n    fqdn: %s.example.com\n    host: %s\n    backend: %s:80\n%s", n, n, host, n, extra)
					}
					body := "services:\n" + svc("plain-resolver", "resolver", "") + svc("plain-appbox", "appbox", "")
					if authHost != "" {
						yaml += "  auth_service: sso\n"
						body += svc("sso", authHost, "")
					}
					if tunnelHost != "" {
						body += svc("pub", tunnelHost, "    tunnel: true\n")
					}
					if err := os.WriteFile(filepath.Join(dir, configName), []byte(yaml+body), 0o644); err != nil {
						t.Fatal(err)
					}
					defer SetLocalHost(func(*config.Config) string { return self })()

					var sink bytes.Buffer
					if code := Doctor(&Env{Out: &sink, Err: &sink, NoTerminal: true}, filepath.Join(dir, configName), []string{"--fix"}); code > 1 {
						t.Fatalf("doctor --fix: %d\n%s", code, sink.String())
					}
					os.Remove(logPath)
					var out bytes.Buffer
					env := &Env{Out: &out, Err: &out, NoTerminal: true}
					if code := Apply(env, dir, filepath.Join(dir, configName), nil); code != 0 {
						t.Fatalf("apply: %d\n%s", code, out.String())
					}

					// What apply really ran, in the same words ApplySteps uses.
					raw, _ := os.ReadFile(logPath)
					var did []string
					for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
						switch {
						case line == "restart pihole":
							did = append(did, "restart pihole")
						case strings.HasPrefix(line, "exec caddy caddy reload"):
							did = append(did, "reload caddy")
						case line == "restart sso":
							did = append(did, "restart the auth provider (sso)")
						case line == "restart cloudflared":
							did = append(did, "restart cloudflared")
						}
					}

					host, steps, err := ApplySteps(dir, filepath.Join(dir, configName))
					if err != nil {
						t.Fatal(err)
					}
					if host != self {
						t.Errorf("host = %q, want %q", host, self)
					}
					sort.Strings(did)
					want := append([]string(nil), steps...)
					sort.Strings(want)
					if strings.Join(did, "|") != strings.Join(want, "|") {
						t.Errorf("ApplySteps says %v but apply did %v", steps, did)
					}
				})
			}
		}
	}
}

// A machine that matches no declared host has nothing to apply.
func TestApplySteps_UnknownMachine(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	defer SetLocalHost(func(*config.Config) string { return "" })()
	host, steps, err := ApplySteps(dir, filepath.Join(dir, configName))
	if err != nil || host != "" || len(steps) != 0 {
		t.Errorf("got host=%q steps=%v err=%v, want nothing", host, steps, err)
	}
}
