package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Missing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "services.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c.Exists {
		t.Error("Exists should be false for a missing file")
	}
	if len(c.Services) != 0 {
		t.Errorf("missing file should yield empty services, got %d", len(c.Services))
	}
}

func TestLoad_Present(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "machines: {}\ndomains: {}\nservices: {}\n")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Exists {
		t.Error("Exists should be true for a present file")
	}
}

func TestLoad_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	write(t, path, "machines: {\n  this is : : not yaml\n")

	if _, err := Load(path); err == nil {
		t.Fatal("malformed yaml should be a fatal error")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")

	c, _ := Load(path)
	c.Hosts["resolver"] = Host{IP: "192.0.2.1", Dir: "resolver"}
	c.Domains["example.com"] = Domain{}
	c.Defaults.DNSHost = "resolver"
	c.Services["docs"] = Service{FQDN: "docs.example.com", Host: "resolver", Backend: "paperless:8000"}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c2.Hosts["resolver"].IP != "192.0.2.1" {
		t.Errorf("host not round-tripped: %+v", c2.Hosts["resolver"])
	}
	if c2.Services["docs"].Backend != "paperless:8000" {
		t.Errorf("service not round-tripped: %+v", c2.Services["docs"])
	}
	if c2.Defaults.DNSHost != "resolver" {
		t.Errorf("defaults not round-tripped: %+v", c2.Defaults)
	}
}

func TestResolvedDir(t *testing.T) {
	// Empty Dir defaults to the host name; an explicit Dir overrides it.
	if got := (Host{}).ResolvedDir("resolver"); got != "resolver" {
		t.Errorf("empty Dir should default to name, got %q", got)
	}
	if got := (Host{Dir: "custom"}).ResolvedDir("resolver"); got != "custom" {
		t.Errorf("explicit Dir should win, got %q", got)
	}
}

func TestDNSHost(t *testing.T) {
	c := &Config{Defaults: Defaults{DNSHost: "resolver"}}
	if got := c.DNSHost(); got != "resolver" {
		t.Errorf("dns_host: got %q want resolver", got)
	}
}

// AtomicWrite must not leave temp files behind on success (design §9).
func TestAtomicWrite_NoTempLeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.conf")
	if err := AtomicWrite(path, []byte("hi")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hi" {
		t.Errorf("content: got %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "out.conf" {
			t.Errorf("stray file left behind: %q", e.Name())
		}
	}
}

func TestServicesUsingHost(t *testing.T) {
	c := &Config{
		Defaults: Defaults{DNSHost: "resolver"},
		Services: map[string]Service{
			"docs":   {FQDN: "docs.example.com", Host: "appbox"},
			"photos": {FQDN: "photos.example.net", Host: "resolver"},
			"vault":  {FQDN: "vault.example.net", Host: "appbox"},
		},
	}
	// appbox runs docs+vault.
	if got := c.ServicesUsingHost("appbox"); len(got) != 2 || got[0] != "docs" || got[1] != "vault" {
		t.Errorf("appbox users: %v", got)
	}
	// resolver is the dns_host — EVERY service depends on it (plus it runs photos).
	got := c.ServicesUsingHost("resolver")
	if len(got) != 3 {
		t.Errorf("resolver (the dns_host) should be used by all 3 services, got %v", got)
	}
	if got := c.ServicesUsingHost("ghost"); len(got) != 0 {
		t.Errorf("unused host should have no users: %v", got)
	}
}

func TestServicesUsingDomain(t *testing.T) {
	c := &Config{Services: map[string]Service{
		"docs":   {FQDN: "docs.example.com"},
		"sub":    {FQDN: "a.b.example.com"},
		"photos": {FQDN: "photos.example.net"},
	}}
	got := c.ServicesUsingDomain("example.com")
	if len(got) != 2 || got[0] != "docs" || got[1] != "sub" {
		t.Errorf("example.com users: %v", got)
	}
	if got := c.ServicesUsingDomain("unused.net"); len(got) != 0 {
		t.Errorf("unused domain should have no users: %v", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
