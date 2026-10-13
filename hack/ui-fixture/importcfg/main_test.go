package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic input only (documentation ranges, example domains): never put a real services.yaml in a test.
const source = `hosts:
  edge:
    ip: 203.0.113.5
    ssh: ops@203.0.113.5
    trusted_proxies: [198.51.100.9/32]
  core:
    ip: 203.0.113.6
domains:
  - real.example.net
  - other.example.org
defaults:
  dns_host: core
  deploy_host: core
  auth_service: sso
services:
  sso:
    fqdn: auth.real.example.net
    host: core
    backend: sso:9091
    tunnel: true
  app:
    fqdn: app.other.example.org
    host: edge
    backend: app:80
    disabled: true
    auth:
      mode: forward
      groups: [admins]
`

func write(t *testing.T, body string) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return src, filepath.Join(dir, "out.yaml")
}

// Everything real is replaced: one host, one domain, no ssh — and the services and their auth survive.
func TestImport_NeutralizesHostsDomainsAndSSH(t *testing.T) {
	src, dst := write(t, source)
	if err := run(src, dst, "example.com", "pi", "172.28.0.10", "172.28.0.13/32"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	out := string(b)
	for _, real := range []string{"real.example.net", "other.example.org", "203.0.113", "ops@", "edge", "core", "198.51.100.9"} {
		if strings.Contains(out, real) {
			t.Errorf("%q survived:\n%s", real, out)
		}
	}
	for _, want := range []string{"auth.example.com", "app.example.com", "172.28.0.10", "172.28.0.13/32", "dns_host: pi", "deploy_host: pi", "mode: forward", "admins", "disabled: true", "tunnel: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// The safety net: if anything from the source would survive (here a backend naming a real domain, which is not
// rewritten), the tool refuses and leaves NO output behind.
func TestImport_RefusesAndWritesNothingWhenSomethingRealSurvives(t *testing.T) {
	leaky := strings.Replace(source, "backend: app:80", "backend: app.real.example.net:80", 1)
	src, dst := write(t, leaky)
	err := run(src, dst, "example.com", "pi", "172.28.0.10", "172.28.0.13/32")
	if err == nil || !strings.Contains(err.Error(), "survived") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("the output file must not be left behind")
	}
}

// Two services that would become the same address once the domains collapse are refused, not silently merged.
func TestImport_RefusesAddressCollisions(t *testing.T) {
	clash := strings.Replace(source, "app.other.example.org", "auth.other.example.org", 1)
	src, dst := write(t, clash)
	if err := run(src, dst, "example.com", "pi", "172.28.0.10", "172.28.0.13/32"); err == nil || !strings.Contains(err.Error(), "both be") {
		t.Fatalf("want a collision error, got %v", err)
	}
}

func TestMoveDomain(t *testing.T) {
	ds := []string{"a.example", "b.example"}
	for in, want := range map[string]string{"x.a.example": "x.example.com", "a.example": "example.com", "y.z.b.example": "y.z.example.com"} {
		if got, err := moveDomain(in, ds, "example.com"); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := moveDomain("x.elsewhere.test", ds, "example.com"); err == nil {
		t.Error("an address outside every declared domain must be refused")
	}
}
