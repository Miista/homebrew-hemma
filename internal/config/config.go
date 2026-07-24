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

// AuthMode is how a service authenticates. Three states:
//
//   - AuthNone ("")     — no auth; a plain reverse_proxy is rendered.
//   - AuthForward ("forward") — Caddy forward-auth: the site imports the (auth)
//     snippet before proxying (hemma adds the auth gate).
//   - AuthOIDC ("oidc") — the app speaks OIDC itself; hemma adds NO forward
//     auth (a PLAIN reverse_proxy), and instead validates read-only that an
//     Authelia OIDC client is registered for the service.
//
// Rendering forward vs oidc differently is deliberate: an oidc service must not
// look identical to a no-auth service in the generated Caddy — that legibility
// gap would hide whether a service is protected.
type AuthMode string

const (
	AuthNone    AuthMode = ""
	AuthForward AuthMode = "forward"
	AuthOIDC    AuthMode = "oidc"
)

// parseAuthMode maps a mode string to an AuthMode. An unrecognized string is
// treated as AuthNone (fail safe: an unknown mode never silently renders as
// protected), and an error is returned so the typo surfaces rather than being
// swallowed.
func parseAuthMode(s string) (AuthMode, error) {
	switch AuthMode(s) {
	case AuthNone, "none":
		return AuthNone, nil
	case AuthForward:
		return AuthForward, nil
	case AuthOIDC:
		return AuthOIDC, nil
	default:
		return AuthNone, fmt.Errorf("unknown auth mode %q — expected forward, oidc, or none", s)
	}
}

// Auth is a service's auth declaration: a mode plus, optionally, the auth
// provider group names allowed to access the service (used to generate the
// provider's access-control rules; groups are meaningless with mode none).
type Auth struct {
	Mode   AuthMode
	Groups []string
}

// authWire is the mapping (long) YAML form of Auth.
type authWire struct {
	Mode   string   `yaml:"mode"`
	Groups []string `yaml:"groups,omitempty"`
}

// UnmarshalYAML accepts three forms, oldest first:
//   - legacy bool  — `auth: true` → forward, `auth: false` → none
//   - string       — `auth: forward` / `auth: oidc` / `auth: none`
//   - mapping      — `auth: {mode: forward, groups: [admins]}`
//
// Back-compat: existing services.yaml files written with the bool or string
// form still round-trip.
func (a *Auth) UnmarshalYAML(value *yaml.Node) error {
	// Try bool first (legacy `auth: true`/`auth: false`).
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			a.Mode = AuthForward
		} else {
			a.Mode = AuthNone
		}
		return nil
	}
	// Mapping form: {mode: ..., groups: [...]}.
	if value.Kind == yaml.MappingNode {
		var w authWire
		if err := value.Decode(&w); err != nil {
			return fmt.Errorf("auth object form: %w", err)
		}
		if w.Mode == "" {
			return fmt.Errorf("auth object form requires a mode (forward, oidc, or none)")
		}
		mode, err := parseAuthMode(w.Mode)
		a.Mode, a.Groups = mode, w.Groups
		return err
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("auth must be a bool, one of forward/oidc/none, or a {mode, groups} mapping: %w", err)
	}
	mode, err := parseAuthMode(s)
	a.Mode = mode
	return err
}

// MarshalYAML emits the SHORT string form (`auth: forward`) when no groups are
// set, and the mapping form only when groups carry data — the YAML stays as
// terse as before this field grew structure.
func (a Auth) MarshalYAML() (any, error) {
	if len(a.Groups) == 0 {
		return string(a.Mode), nil
	}
	return authWire{Mode: string(a.Mode), Groups: a.Groups}, nil
}

// IsZero lets yaml's omitempty drop `auth:` entirely for unprotected services
// (mode none, no groups), matching the pre-struct behavior.
func (a Auth) IsZero() bool { return a.Mode == AuthNone && len(a.Groups) == 0 }

// Host is one host in the homelab, owning a directory in the repo. The
// directory defaults to the host's name (its key in the hosts map); the dir
// field need only be set for the rare case where they differ. Generated-file
// subpaths under that directory are fixed (DefaultDnsmasqDir / DefaultCaddySitesDir).
type Host struct {
	IP  string `yaml:"ip"`
	Dir string `yaml:"dir,omitempty"`
	// SSH is an optional VERBATIM ssh(1) destination (an ssh_config alias,
	// user@host, anything ssh accepts) used by `hemma deploy` to reach this
	// host. There are deliberately no ssh_user/ssh_port fields — ssh_config
	// owns that machinery. Empty means the host's NAME is the destination
	// (consistent with the name == repo-dir convention).
	SSH string `yaml:"ssh,omitempty"`
}

// ResolvedDir returns the host's repo directory: the explicit Dir if set,
// else the host name (the convention is dir == name).
func (m Host) ResolvedDir(name string) string {
	if m.Dir != "" {
		return m.Dir
	}
	return name
}

// SSHDest returns the ssh(1) destination `hemma deploy` uses to reach the
// host: the explicit SSH field if set, else the host name (the convention is
// destination == name, mirroring ResolvedDir).
func (m Host) SSHDest(name string) string {
	if m.SSH != "" {
		return m.SSH
	}
	return name
}

// Domain is a registrable domain hemma manages. The TLS snippet name and cert
// paths are derived from the domain (see render.TLSSnippetName / TLSSnippet),
// so no per-domain configuration is needed.
type Domain struct{}

// Defaults holds repo-wide defaults.
type Defaults struct {
	DNSHost string `yaml:"dns_host"`
	// AuthSnippet is an optional repo-relative path to a Caddy file whose
	// contents are the auth directive(s) copied into the generated (auth)
	// snippet on every host. Empty means an empty (auth) {} stub is generated
	// (no-op), so services that `import auth` remain valid but unprotected.
	AuthSnippet string `yaml:"auth_snippet,omitempty"`
	// AuthService names the service that IS the forward-auth backend (e.g. an
	// Authelia portal). Its site block gets `header_up X-Forwarded-Host
	// {header.X-Forwarded-Host}` so the original request host survives the
	// hairpin through Caddy — without it, forward-auth reconstructs the auth
	// domain as the target and post-login redirects loop back to the portal.
	// Parallels dns_host: one repo-wide role, named by service, set via
	// `hemma set auth-service <name>`.
	AuthService string `yaml:"auth_service,omitempty"`
	// PublicLabel is the docker-compose label key whose presence on a
	// container declares public (tunnel) ingress for an FQDN. Read-only and
	// display-only: `list` uses it to tell local-only services from publicly
	// reachable ones (design §12). It is configurable — rather than
	// hardcoded — so hemma stays generator-agnostic: the default matches
	// cloudflared-wrapper's convention on this homelab, but any tunnel tool
	// that declares ingress by compose label works by setting this key.
	// Set to "none" to switch the EXPOSURE column off entirely.
	// Empty means DefaultPublicLabel.
	PublicLabel string `yaml:"public_label,omitempty"`
}

// DefaultPublicLabel is the compose label key consulted when
// defaults.public_label is unset — cloudflared-wrapper's convention.
const DefaultPublicLabel = "cloudflare.io/hostname"

// PublicLabelDisabled is the defaults.public_label value that turns the
// public/local distinction off (no EXPOSURE column, no compose reads).
const PublicLabelDisabled = "none"

// ResolvedPublicLabel returns the compose label key to consult, or "" when the
// feature is switched off.
func (d Defaults) ResolvedPublicLabel() string {
	switch d.PublicLabel {
	case "":
		return DefaultPublicLabel
	case PublicLabelDisabled:
		return ""
	}
	return d.PublicLabel
}

// Service is one declared service entry. There is no per-service dns_host:
// every record is served by the single resolver (defaults.dns_host).
type Service struct {
	FQDN     string `yaml:"fqdn"`
	Host     string `yaml:"host"`
	Backend  string `yaml:"backend"`
	Disabled bool   `yaml:"disabled,omitempty"`
	// Auth is how this service authenticates (none/forward/oidc, see AuthMode).
	// - forward opts into the (auth) snippet: its site block imports (auth)
	//   before proxying. The snippet content is repo-global (defaults.auth_snippet).
	// - oidc means the app does OIDC itself; hemma renders a plain
	//   reverse_proxy and instead verifies an Authelia OIDC client exists.
	// omitempty (via Auth.IsZero) drops the unprotected zero value; forward/oidc
	// serialize as their string form, or the {mode, groups} mapping when groups
	// are set. Legacy `auth: true` is accepted on load and re-emitted as
	// `auth: forward`.
	Auth Auth `yaml:"auth,omitempty"`
	// PublicPaths is a list of URL paths that are exempt from auth, served
	// directly by the backend without going through the forward-auth gate.
	// Only meaningful when Auth == AuthForward; ignored otherwise.
	PublicPaths []string `yaml:"public_paths,omitempty"`
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

	// AuthSnippetBody is the content read from defaults.auth_snippet, used as
	// the body of the generated (auth) snippet. Empty when no auth_snippet is
	// configured. Populated by LoadAuthSnippet, not from services.yaml itself.
	AuthSnippetBody string `yaml:"-"`

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

// LoadAuthSnippet reads the file referenced by defaults.auth_snippet (resolved
// relative to repoRoot) into AuthSnippetBody. It is a no-op when no auth_snippet
// is set. On a read error it returns the error WITHOUT clearing AuthSnippetBody,
// so callers can keep the last-good generated snippet in place rather than
// silently reverting every service to unprotected — a path typo must never
// disable auth fleet-wide (report-but-proceed, design §8).
func (c *Config) LoadAuthSnippet(repoRoot string) error {
	if c.Defaults.AuthSnippet == "" {
		c.AuthSnippetBody = ""
		return nil
	}
	path := c.Defaults.AuthSnippet
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read auth_snippet %s: %w", c.Defaults.AuthSnippet, err)
	}
	c.AuthSnippetBody = string(data)
	return nil
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
