// Package config loads, mutates, and persists services.yaml, the sole source
// of truth. The tool owns this file and rewrites it wholesale on mutation;
// ordering and human comments are not preserved.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default output-path segments (design §4, §10). Per-host config may
// override these so layout is data, not hardcoded.
const (
	DefaultDnsmasqDir    = "pihole/data/dnsmasq.d/generated"
	DefaultCaddySitesDir = "caddy/data/sites"
)

// Host is one host in the homelab, owning a directory in the repo. The
// directory defaults to the host's name (its key in the hosts map); the dir
// field need only be set for the rare case where they differ. Generated-file
// subpaths under that directory are fixed (DefaultDnsmasqDir / DefaultCaddySitesDir).
type Host struct {
	IP  string `yaml:"ip"`
	Dir string `yaml:"dir,omitempty"`
}

// ResolvedDir returns the host's repo directory: the explicit Dir if set,
// else the host name (the convention is dir == name).
func (m Host) ResolvedDir(name string) string {
	if m.Dir != "" {
		return m.Dir
	}
	return name
}

// Domain maps a registrable domain to the tls snippet its Caddyfile imports.
type Domain struct {
	TLSImport string `yaml:"tls_import"`
}

// Defaults holds repo-wide defaults.
type Defaults struct {
	DNSHost string `yaml:"dns_host"`
}

// Service is one declared service entry.
type Service struct {
	FQDN    string `yaml:"fqdn"`
	Host    string `yaml:"host"`
	Backend string `yaml:"backend"`
	DNSHost string `yaml:"dns_host,omitempty"` // optional per-service override
}

// Config is the in-memory representation of services.yaml.
type Config struct {
	Hosts    map[string]Host    `yaml:"hosts"`
	Domains  map[string]Domain  `yaml:"domains"`
	Defaults Defaults           `yaml:"defaults"`
	Services map[string]Service `yaml:"services"`

	// Exists reports whether services.yaml was present on disk at load time.
	// Missing (Exists=false) is distinct from present-but-empty: add creates
	// the file, but sync/update/remove should refuse and guide the user (§8).
	Exists bool `yaml:"-"`

	path string `yaml:"-"`
}

// Load reads and parses services.yaml. A parse failure is the one globally
// fatal condition (design §7).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := &Config{
				Hosts:    map[string]Host{},
				Domains:  map[string]Domain{},
				Services: map[string]Service{},
				Exists:   false,
				path:     path,
			}
			return c, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Hosts == nil {
		c.Hosts = map[string]Host{}
	}
	if c.Domains == nil {
		c.Domains = map[string]Domain{}
	}
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	c.Exists = true
	c.path = path
	return &c, nil
}

// Save rewrites services.yaml wholesale. Owned file; ordering not preserved.
func (c *Config) Save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicWrite(c.path, data)
}

// DNSHostFor returns the resolved dns_host for a service (override or default).
func (c *Config) DNSHostFor(s Service) string {
	if s.DNSHost != "" {
		return s.DNSHost
	}
	return c.Defaults.DNSHost
}

// ServicesUsingHost returns the names of services that reference host
// `name` as their host or resolved dns_host (sorted). A host is also
// considered referenced if it is the defaults.dns_host and any service relies
// on that default.
func (c *Config) ServicesUsingHost(name string) []string {
	var out []string
	for svc, s := range c.Services {
		if s.Host == name || c.DNSHostFor(s) == name {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

// ServicesUsingDomain returns the names of services whose fqdn matches domain
// `name` exactly or as a suffix (sorted).
func (c *Config) ServicesUsingDomain(name string) []string {
	var out []string
	for svc, s := range c.Services {
		if s.FQDN == name || strings.HasSuffix(s.FQDN, "."+name) {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

// atomicWrite writes to a temp file in the same dir, fsyncs, then renames
// (design §9).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// AtomicWrite exposes atomic file writing for the sync engine.
func AtomicWrite(path string, data []byte) error { return atomicWrite(path, data) }
