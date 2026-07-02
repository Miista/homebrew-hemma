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
	// Directly in dnsmasq.d/ — pihole's conf-dir=...,*.conf does NOT recurse
	// into subdirectories, so a 'generated/' subdir would be silently ignored.
	// The "generated" marker lives in the filename (<service>.generated.conf).
	DefaultDnsmasqDir    = "pihole/data/dnsmasq.d"
	DefaultCaddyDataDir  = "caddy/data"
	DefaultCaddySitesDir = "caddy/data/sites"
	DefaultCaddyTLSDir   = "caddy/data/tls"
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

// Domain is a registrable domain mirage manages. The TLS snippet name and cert
// paths are derived from the domain (see render.TLSSnippetName / TLSSnippet),
// so no per-domain configuration is needed.
type Domain struct{}

// Defaults holds repo-wide defaults.
type Defaults struct {
	DNSHost string `yaml:"dns_host"`
}

// Service is one declared service entry. There is no per-service dns_host:
// every record is served by the single resolver (defaults.dns_host).
type Service struct {
	FQDN     string `yaml:"fqdn"`
	Host     string `yaml:"host"`
	Backend  string `yaml:"backend"`
	Disabled bool   `yaml:"disabled,omitempty"`
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

// wireConfig is the YAML-serialized form of Config. Domains are a plain list
// of strings (no per-domain data), which is cleaner than map[string]Domain{}.
type wireConfig struct {
	Hosts    map[string]Host    `yaml:"hosts"`
	Domains  []string           `yaml:"domains"`
	Defaults Defaults           `yaml:"defaults"`
	Services map[string]Service `yaml:"services"`
}

// Load reads and parses services.yaml. A parse failure is the one globally
// fatal condition (design §7).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{
				Hosts:    map[string]Host{},
				Domains:  map[string]Domain{},
				Services: map[string]Service{},
				path:     path,
			}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var w wireConfig
	if err := yaml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c := &Config{
		Hosts:    w.Hosts,
		Domains:  map[string]Domain{},
		Defaults: w.Defaults,
		Services: w.Services,
		Exists:   true,
		path:     path,
	}
	for _, d := range w.Domains {
		c.Domains[d] = Domain{}
	}
	if c.Hosts == nil {
		c.Hosts = map[string]Host{}
	}
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	return c, nil
}

// Save rewrites services.yaml wholesale. Owned file; ordering not preserved.
func (c *Config) Save() error {
	domains := make([]string, 0, len(c.Domains))
	for d := range c.Domains {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	w := wireConfig{
		Hosts:    c.Hosts,
		Domains:  domains,
		Defaults: c.Defaults,
		Services: c.Services,
	}
	data, err := yaml.Marshal(w)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicWrite(c.path, data)
}

// DNSHost returns the single resolver host for all records (defaults.dns_host).
func (c *Config) DNSHost() string {
	return c.Defaults.DNSHost
}

// MatchDomain returns the longest registrable domain in the domains map that
// fqdn matches (exact or as a suffix), and whether any matched. e.g.
// docs.example.com matches domain example.com.
func (c *Config) MatchDomain(fqdn string) (string, bool) {
	best := ""
	for dom := range c.Domains {
		if (fqdn == dom || strings.HasSuffix(fqdn, "."+dom)) && len(dom) > len(best) {
			best = dom
		}
	}
	return best, best != ""
}

// DomainNames returns the defined domain names, sorted.
func (c *Config) DomainNames() []string {
	out := make([]string, 0, len(c.Domains))
	for d := range c.Domains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// ServicesUsingHost returns the names of services that depend on host `name`
// (sorted): either it runs the service, or it is the resolver (dns_host) that
// every service's DNS record is routed through.
func (c *Config) ServicesUsingHost(name string) []string {
	resolver := c.Defaults.DNSHost
	var out []string
	for svc, s := range c.Services {
		if s.Host == name || name == resolver {
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
