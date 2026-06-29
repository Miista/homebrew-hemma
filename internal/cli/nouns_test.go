package cli

import (
	"os"
	"path/filepath"
	"testing"

	"shd/internal/config"
)

func load(t *testing.T, dir string) *config.Config {
	t.Helper()
	c, err := config.Load(filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHostAdd_CreatesHost(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver")
	code := Run([]string{"-C", dir, "host", "add", "resolver", "--ip", "192.0.2.1"})
	if code != 0 {
		t.Fatalf("host add should exit 0, got %d", code)
	}
	c := load(t, dir)
	h := c.Hosts["resolver"]
	// Dir is left empty and defaults to the host name.
	if h.IP != "192.0.2.1" || h.Dir != "" || h.ResolvedDir("resolver") != "resolver" {
		t.Errorf("host not stored as expected: %+v", h)
	}
}

func TestHostAdd_RequiresIP(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver")
	if code := Run([]string{"-C", dir, "host", "add", "resolver"}); code != 2 {
		t.Errorf("host add without --ip should exit 2, got %d", code)
	}
}

func TestHostAdd_Duplicate(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver")
	Run([]string{"-C", dir, "host", "add", "resolver", "--ip", "192.0.2.1"})
	if code := Run([]string{"-C", dir, "host", "add", "resolver", "--ip", "1.2.3.4"}); code != 1 {
		t.Errorf("duplicate host add should exit 1, got %d", code)
	}
}

func TestHostRemove_RefusesWhenReferenced(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir) // defines resolver + appbox, dns_host resolver
	Run([]string{"-C", dir, "add", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})

	// appbox is host of docs -> refuse.
	if code := Run([]string{"-C", dir, "host", "remove", "appbox"}); code != 1 {
		t.Errorf("removing referenced host should exit 1, got %d", code)
	}
	if _, ok := load(t, dir).Hosts["appbox"]; !ok {
		t.Error("referenced host should not have been removed")
	}

	// resolver is the dns_host of docs (via defaults) -> also refuse.
	if code := Run([]string{"-C", dir, "host", "remove", "resolver"}); code != 1 {
		t.Errorf("removing dns_host should exit 1, got %d", code)
	}
}

func TestHostRemove_Unreferenced(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "spare")
	Run([]string{"-C", dir, "host", "add", "spare", "--ip", "10.0.9.9"})
	if code := Run([]string{"-C", dir, "host", "remove", "spare"}); code != 0 {
		t.Errorf("removing unreferenced host should exit 0, got %d", code)
	}
	if _, ok := load(t, dir).Hosts["spare"]; ok {
		t.Error("host should have been removed")
	}
}

func TestDomainAdd_CreatesDomain(t *testing.T) {
	dir := t.TempDir()
	code := Run([]string{"-C", dir, "domain", "add", "example.com", "--tls-import", "tls_example_com"})
	if code != 0 {
		t.Fatalf("domain add should exit 0, got %d", code)
	}
	if load(t, dir).Domains["example.com"].TLSImport != "tls_example_com" {
		t.Error("domain not stored")
	}
}

func TestDomainRemove_RefusesWhenReferenced(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	Run([]string{"-C", dir, "add", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})

	if code := Run([]string{"-C", dir, "domain", "remove", "example.com"}); code != 1 {
		t.Errorf("removing referenced domain should exit 1, got %d", code)
	}
	if _, ok := load(t, dir).Domains["example.com"]; !ok {
		t.Error("referenced domain should not have been removed")
	}
}

// End-to-end bootstrap entirely via CLI: host -> domain -> service -> sync.
func TestBootstrap_ViaCLIOnly(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "appbox", "resolver")
	steps := [][]string{
		{"-C", dir, "host", "add", "appbox", "--ip", "192.0.2.2"},
		{"-C", dir, "host", "add", "resolver", "--ip", "192.0.2.1"},
		{"-C", dir, "domain", "add", "example.com", "--tls-import", "tls_example_com"},
		{"-C", dir, "add", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000", "--dns-host", "resolver"},
	}
	for _, s := range steps {
		if code := Run(s); code != 0 {
			t.Fatalf("step %v exited %d", s, code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "appbox", "caddy/data/sites/docs.caddy")); err != nil {
		t.Errorf("bootstrap should produce a generated caddy site: %v", err)
	}
}

// --- dns_host bootstrap + dns-host set command ---

// host add must NOT auto-set defaults.dns_host (that magic was removed in
// favor of an explicit dns-host set + a sync-time refusal).
func TestHostAdd_DoesNotSetDefaultDNSHost(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "appbox")
	Run([]string{"-C", dir, "host", "add", "appbox", "--ip", "192.0.2.2"})
	if got := load(t, dir).Defaults.DNSHost; got != "" {
		t.Errorf("host add should not set default dns_host, got %q", got)
	}
}

// host add must refuse a name with no matching directory in the repo (the
// host name must equal an existing dir; otherwise it's a typo).
func TestHostAdd_MissingDirRejected(t *testing.T) {
	dir := t.TempDir() // no "appbox" dir created
	if code := Run([]string{"-C", dir, "host", "add", "appbox", "--ip", "192.0.2.2"}); code != 1 {
		t.Errorf("host add with no matching dir should exit 1, got %d", code)
	}
	if _, ok := load(t, dir).Hosts["appbox"]; ok {
		t.Error("host with no matching dir must not be persisted")
	}
}

// sync must refuse when no dns_host is resolvable, rather than skip silently.
func TestSync_RefusesWithoutDNSHost(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "appbox")
	Run([]string{"-C", dir, "host", "add", "appbox", "--ip", "192.0.2.2"})
	Run([]string{"-C", dir, "domain", "add", "example.com", "--tls-import", "tls_example_com"})
	// add triggers a sync; with no dns_host set it must refuse (exit 1).
	if code := Run([]string{"-C", dir, "add", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 1 {
		t.Errorf("sync without dns_host should exit 1, got %d", code)
	}
	// After setting it, sync succeeds.
	Run([]string{"-C", dir, "dns-host", "set", "appbox"})
	if code := Run([]string{"-C", dir, "sync"}); code != 0 {
		t.Errorf("sync after dns-host set should exit 0, got %d", code)
	}
}

func TestDNSHostSet(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir) // defines resolver + appbox, default dns_host resolver
	if code := Run([]string{"-C", dir, "dns-host", "set", "appbox"}); code != 0 {
		t.Fatalf("dns-host set to existing host should exit 0, got %d", code)
	}
	if got := load(t, dir).Defaults.DNSHost; got != "appbox" {
		t.Errorf("dns_host not updated, got %q", got)
	}
}

func TestDNSHostSet_UnknownHostRejected(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "dns-host", "set", "ghost"}); code != 1 {
		t.Errorf("dns-host set to unknown host should exit 1, got %d", code)
	}
}

func TestDNSHostSet_MissingArgs(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	if code := Run([]string{"-C", dir, "dns-host"}); code != 2 {
		t.Errorf("dns-host with no subcommand should exit 2, got %d", code)
	}
	if code := Run([]string{"-C", dir, "dns-host", "set"}); code != 2 {
		t.Errorf("dns-host set with no name should exit 2, got %d", code)
	}
}
