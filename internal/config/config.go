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

// AuthMode is how a service authenticates. Four states:
//
//   - AuthNone ("")     — no auth; a plain reverse_proxy is rendered.
//   - AuthForward ("forward") — Caddy forward-auth: the site imports the (auth)
//     snippet before proxying (hemma adds the auth gate via Authelia).
//   - AuthOIDC ("oidc") — Authelia, but the app speaks OIDC itself; hemma adds
//     NO forward auth (a PLAIN reverse_proxy), and instead validates read-only
//     that an Authelia OIDC client is registered for the service.
//   - AuthExternal ("external") — authenticated by something that is NOT
//     Authelia: the app's own login, an API token, an edge proxy (e.g.
//     Cloudflare Access). Renders identically to oidc/none (plain
//     reverse_proxy), and hemma verifies NOTHING — the auth lives outside its
//     provider boundary, so there is nothing for it to check. Purely a
//     declaration, so `list` can show "authenticated, just not here" instead of
//     an ambiguous "-".
//
// oidc and external render the same but mean opposites: oidc IS Authelia (hence
// its client check), external is explicitly NOT Authelia (hence no check). The
// point of both is legibility — a protected service must not look identical to
// an unprotected one in the generated Caddy or in `list`.
type AuthMode string

const (
	AuthNone     AuthMode = ""
	AuthForward  AuthMode = "forward"
	AuthOIDC     AuthMode = "oidc"
	AuthExternal AuthMode = "external"
)

// UsesProvider reports whether this mode involves hemma's auth provider
// (Authelia) at all. forward and oidc do — they generate access-control rules
// or require a registered OIDC client, and so are handed to the provider for
// validation. none and external do NOT: none has no auth, and external declares
// auth that lives outside Authelia entirely. This is the single predicate every
// provider-facing site branches on, so "does Authelia touch this service?" has
// one answer rather than four scattered comparisons.
func (m AuthMode) UsesProvider() bool {
	return m == AuthForward || m == AuthOIDC
}

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
	case AuthExternal:
		return AuthExternal, nil
	default:
		return AuthNone, fmt.Errorf("unknown auth mode %q — expected forward, oidc, external, or none", s)
	}
}

// Auth is a service's auth declaration: a mode plus, optionally, the auth
// provider group names allowed to access the service (used to generate the
// provider's access-control rules; groups are meaningless with mode none) and
// the URL paths exempt from the gate.
type Auth struct {
	Mode   AuthMode
	Groups []string
	// BypassPaths lists URL paths served without passing the auth gate. It
	// lives under `auth` because it is only meaningful with mode forward —
	// it was a top-level `public_paths` before (migrated on load, see
	// Service.UnmarshalYAML), which put it at the wrong altitude and collided
	// with the unrelated `public` field. The name mirrors the provider's own
	// vocabulary: these render as Authelia `policy: 'bypass'` rules.
	BypassPaths []string
	// Policy is the per-horizon auth requirement (§4.8): what a request must
	// satisfy depending on whether its resolved client IP is in
	// defaults.local_networks or not. Unset means one requirement for everyone
	// (one_factor), which is what `auth: forward` alone has always meant — so
	// this is purely additive.
	//
	// The cases it exists for: a media server open in the living room but gated
	// from outside ({local: bypass, public: one_factor}), and an admin panel
	// that wants a login at home and a second factor from outside
	// ({local: one_factor, public: two_factor}). Per-service and unset by
	// default on purpose — a repo-wide "trust the LAN" switch would silently
	// unauthenticate every admin panel the moment it landed.
	//
	// Only meaningful with mode forward: it parameterises the forward-auth gate,
	// and oidc/external/none have no gate here to vary.
	//
	// Depends on the reverse proxy resolving the TRUE client IP (§4.7). Without
	// that, the resolved IP is the nearest proxy's, which is not in
	// local_networks, so every request falls to the public policy — it fails
	// toward MORE auth, never less.
	Policy AuthPolicy
	// ForwardGate puts a forward-auth gate in front of an oidc service for
	// requests from outside local_networks (§4.9). An oidc app's own session
	// outlives the provider's, so without it a tunnelled app stays logged in
	// from the internet for as long as the app likes. Valid only with mode oidc
	// and tunnel (plan refuses anything else); named for the mechanism, the
	// horizon follows from the tunnel requirement.
	ForwardGate bool
}

// AuthPolicy is the per-horizon auth requirement. An empty level means "not
// set": that horizon falls back to DefaultPolicy, so a partial map
// (`policy: {public: two_factor}`) overrides one horizon and leaves the other
// at the default rather than being an error.
type AuthPolicy struct {
	// Local applies when the resolved client IP is in defaults.local_networks.
	Local PolicyLevel
	// Public applies to every other request — the catch-all rule.
	Public PolicyLevel
}

// IsZero lets omitempty drop `policy:` when neither horizon is set.
func (p AuthPolicy) IsZero() bool { return p.Local == "" && p.Public == "" }

// PolicyLevel is one Authelia access-control policy value.
type PolicyLevel string

const (
	PolicyBypass    PolicyLevel = "bypass"
	PolicyOneFactor PolicyLevel = "one_factor"
	PolicyTwoFactor PolicyLevel = "two_factor"
	PolicyDeny      PolicyLevel = "deny"
)

// DefaultPolicy is what an unset horizon gets — what `auth: forward` on its own
// has always generated.
const DefaultPolicy = PolicyOneFactor

// ResolveLocal / ResolvePublic return the effective level per horizon.
func (p AuthPolicy) ResolveLocal() PolicyLevel  { return orDefault(p.Local) }
func (p AuthPolicy) ResolvePublic() PolicyLevel { return orDefault(p.Public) }

func orDefault(l PolicyLevel) PolicyLevel {
	if l == "" {
		return DefaultPolicy
	}
	return l
}

// ForService returns the policy as it applies to a service. Without the tunnel
// nothing from the internet reaches it: whoever is outside local_networks got
// in some other way (a VPN, a guest VLAN) and is meant to have the same
// experience as the LAN, so an unset public level follows the local one.
func (p AuthPolicy) ForService(tunnel bool) AuthPolicy {
	if !tunnel && p.Public == "" {
		p.Public = p.Local
	}
	return p
}

// GateScope is where Caddy asks the auth provider for a service (§4.9). One rule covers every mode: Caddy asks only on a
// horizon where the provider has something to check, so a horizon whose effective level is bypass is never sent to it.
type GateScope int

const (
	GateNone    GateScope = iota // never: no provider gate in Caddy (none, external, oidc without forward_gate, bypass on both horizons)
	GateAll                      // every request (`import auth`)
	GateOutside                  // only requests from outside local_networks
)

// GateScope returns the service's scope. Without local_networks there is no local horizon to leave out, so a gate that would
// skip it asks everyone instead — toward more auth, never less.
func (s Service) GateScope(hasLocalNetworks bool) GateScope {
	outside := GateAll
	if hasLocalNetworks {
		outside = GateOutside
	}
	switch {
	case s.Auth.Mode == AuthOIDC && s.Auth.ForwardGate:
		return outside
	case s.Auth.Mode != AuthForward:
		return GateNone
	}
	pol := s.Auth.Policy.ForService(s.Tunnel)
	switch {
	case pol.ResolveLocal() != PolicyBypass:
		return GateAll // the public level is never weaker than the local one, so both horizons are asked
	case pol.ResolvePublic() == PolicyBypass:
		return GateNone // bypass on both horizons: nothing to ask anyone (and doctor flags it, §6.4)
	}
	return outside
}

// strictness orders the levels by how much they ask of a request: bypass asks
// nothing, deny lets nothing through.
func (l PolicyLevel) strictness() int {
	switch l {
	case PolicyBypass:
		return 0
	case PolicyOneFactor:
		return 1
	case PolicyTwoFactor:
		return 2
	default: // deny
		return 3
	}
}

// PublicWeakerThanLocal reports whether the effective public level asks less
// than the local one — a request from outside would get an easier gate than
// one from the LAN, which is never intended. Unset resolves to the default
// first, so `local: two_factor` with no public is caught too.
func (p AuthPolicy) PublicWeakerThanLocal() bool {
	return p.ResolvePublic().strictness() < p.ResolveLocal().strictness()
}

// NeedsLocalRule reports whether the local horizon differs from the public one
// and therefore needs its own networks-scoped rule. Identical levels would
// generate a rule that changes nothing, so it is omitted — the catch-all
// already says the same thing.
func (p AuthPolicy) NeedsLocalRule() bool { return p.ResolveLocal() != p.ResolvePublic() }

// parsePolicyLevel maps a policy string to a PolicyLevel. An unrecognized value
// is an error rather than a default: every wrong guess here is a security
// statement, and silently widening (bypass) or narrowing (deny) what the
// operator wrote is worse than refusing the entry.
func parsePolicyLevel(s string) (PolicyLevel, error) {
	switch PolicyLevel(s) {
	case PolicyBypass:
		return PolicyBypass, nil
	case PolicyOneFactor:
		return PolicyOneFactor, nil
	case PolicyTwoFactor:
		return PolicyTwoFactor, nil
	case PolicyDeny:
		return PolicyDeny, nil
	default:
		return "", fmt.Errorf("unknown auth policy %q — expected bypass, one_factor, two_factor, or deny", s)
	}
}

// policyWire is the YAML form of AuthPolicy.
type policyWire struct {
	Local  string `yaml:"local,omitempty"`
	Public string `yaml:"public,omitempty"`
}

// UnmarshalYAML decodes the policy map, validating both levels.
func (p *AuthPolicy) UnmarshalYAML(value *yaml.Node) error {
	var w policyWire
	if err := value.Decode(&w); err != nil {
		return fmt.Errorf("auth policy: %w", err)
	}
	for _, f := range []struct {
		horizon string
		raw     string
		dst     *PolicyLevel
	}{
		{"local", w.Local, &p.Local},
		{"public", w.Public, &p.Public},
	} {
		if f.raw == "" {
			continue
		}
		lvl, err := parsePolicyLevel(f.raw)
		if err != nil {
			return fmt.Errorf("auth policy %s: %w", f.horizon, err)
		}
		*f.dst = lvl
	}
	return nil
}

// MarshalYAML emits only the horizons that are set.
func (p AuthPolicy) MarshalYAML() (any, error) {
	return policyWire{Local: string(p.Local), Public: string(p.Public)}, nil
}

// authWire is the mapping (long) YAML form of Auth, as written.
type authWire struct {
	Mode        string     `yaml:"mode"`
	Groups      []string   `yaml:"groups,omitempty"`
	BypassPaths []string   `yaml:"bypass_paths,omitempty"`
	Policy      AuthPolicy `yaml:"policy,omitempty"`
	ForwardGate bool       `yaml:"forward_gate,omitempty"`
}

// authRead is the same mapping as read; Mode is a pointer so a missing key can be told from an empty one.
type authRead struct {
	Mode        *string    `yaml:"mode"`
	Groups      []string   `yaml:"groups,omitempty"`
	BypassPaths []string   `yaml:"bypass_paths,omitempty"`
	Policy      AuthPolicy `yaml:"policy,omitempty"`
	ForwardGate bool       `yaml:"forward_gate,omitempty"`
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
		var w authRead
		if err := value.Decode(&w); err != nil {
			return fmt.Errorf("auth object form: %w", err)
		}
		// A MISSING mode is still an error: `auth: {groups: [x]}` must not silently read as "no auth".
		// An EXPLICIT empty one (`mode: ""`) is accepted as none: it is what hemma itself wrote before
		// MarshalYAML spelled the none mode out, so it must stay readable.
		if w.Mode == nil {
			return fmt.Errorf("auth object form requires a mode (forward, oidc, external, or none)")
		}
		mode, err := parseAuthMode(*w.Mode)
		a.Mode, a.Groups, a.BypassPaths, a.Policy, a.ForwardGate = mode, w.Groups, w.BypassPaths, w.Policy, w.ForwardGate
		return err
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("auth must be a bool, one of forward/oidc/external/none, or a {mode, groups} mapping: %w", err)
	}
	mode, err := parseAuthMode(s)
	a.Mode = mode
	return err
}

// MarshalYAML emits the SHORT string form (`auth: forward`) when neither groups
// nor bypass paths are set, and the mapping form only when one of them carries
// data — the YAML stays as terse as before this field grew structure.
func (a Auth) MarshalYAML() (any, error) {
	if len(a.Groups) == 0 && len(a.BypassPaths) == 0 && a.Policy.IsZero() && !a.ForwardGate {
		return string(a.Mode), nil
	}
	// The none mode is the empty string internally; in the mapping form it is spelled out, because the
	// reader requires a mode there. Writing the raw "" made hemma emit a file it then refused to load.
	mode := string(a.Mode)
	if a.Mode == AuthNone {
		mode = "none"
	}
	return authWire{Mode: mode, Groups: a.Groups, BypassPaths: a.BypassPaths, Policy: a.Policy, ForwardGate: a.ForwardGate}, nil
}

// IsZero lets yaml's omitempty drop `auth:` entirely for unprotected services
// (mode none, no groups, no bypass paths), matching the pre-struct behavior.
func (a Auth) IsZero() bool {
	return a.Mode == AuthNone && len(a.Groups) == 0 && len(a.BypassPaths) == 0 && a.Policy.IsZero() && !a.ForwardGate
}

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
	// TrustedProxies is an optional list of extra IPs/CIDRs this host's Caddy
	// should trust for forwarded-header client-IP resolution (Caddyfile's
	// global servers > trusted_proxies option), beyond every other declared
	// host's IP (trusted automatically — see render.TrustedProxiesSnippet).
	// Typically just this host's own cloudflared container IP. Empty means
	// only the other hosts are trusted.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty"`
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
	// `hemma defaults set auth-service <name>`.
	AuthService string `yaml:"auth_service,omitempty"`
	// TunnelDir is the ONE repo-wide path every host's cloudflared container
	// mounts as /etc/cloudflared — hemma writes each host's config.yml there
	// wholesale (design §12: the public horizon is no longer a foreign
	// hand-maintained file once hemma is its sole author, same posture as a
	// Caddy site file). A single value, not per-host: the two real hosts this
	// tracks were briefly misaligned (one mounted cloudflared-local/data, the
	// other cloudflared) and were reconciled to match rather than carrying a
	// per-host override — simpler, and correct as long as every host's
	// compose is kept in that convention. Unlike dns_host, empty here is NOT
	// auto-filled with DefaultTunnelDir: a public service on ANY host with
	// tunnel_dir unset is a hard plan error, not a guessed path — see
	// ResolvedTunnelDir.
	TunnelDir string `yaml:"tunnel_dir,omitempty"`
	// DeployHost names the single host `hemma deploy` fans out FROM. It exists
	// because deploy-readiness is a per-ORIGIN question, not a per-host one:
	// only the origin needs every other host in its known_hosts, and on a
	// segmented network the other hosts may deliberately have no route to their
	// peers at all. Without this, doctor reports a missing known_hosts entry on
	// every host, and on a non-origin that finding is unfixable by design —
	// a permanently red doctor teaches you to ignore it.
	//
	// Empty means unset: the readiness check then runs everywhere, which is the
	// pre-existing behaviour, so adopting the field is opt-in. Parallels
	// dns_host and auth_service: one repo-wide role, named by host, set via
	// `hemma defaults set deploy-host <name>`.
	DeployHost string `yaml:"deploy_host,omitempty"`
	// LocalNetworks are the CIDRs that count as the LOCAL horizon when a service
	// declares a per-horizon auth.policy (§4.8) — the home network, in resolved
	// client-IP terms. Repo-wide because it describes the network, not any one
	// service: a VLAN renumber is one edit here rather than N across services.
	// WHICH services vary their policy by horizon is per-service, so this list
	// grants nothing on its own.
	//
	// Unused when no service declares a differing local policy. Declared but
	// EMPTY while a service does is reported (plan.UnresolvedLocalPolicy), not
	// fatal: every other file is unaffected and the service's public policy
	// still renders as its catch-all, so the only consequence is the local
	// horizon getting the stricter treatment — the safe direction.
	LocalNetworks []string `yaml:"local_networks,omitempty"`
}

// DefaultTunnelDir is the conventional cloudflared config directory name —
// a SUGGESTION surfaced in help text and error messages (e.g. "hemma set
// tunnel-dir <host> cloudflared"), not an automatic fallback: there is no
// method that applies it silently. A host's tunnel_dir must be set,
// explicitly, by a human who has confirmed what that host's compose file
// actually mounts — verified tonight that two real hosts genuinely differ
// (cloudflared-local/data vs cloudflared), so guessing would as often be
// wrong as right.
const DefaultTunnelDir = "cloudflared"

// ResolvedTunnelDir returns the directory (relative to each host's own repo
// dir) where hemma writes that host's cloudflared config.yml, and whether it
// was actually configured. Empty is NOT auto-filled with DefaultTunnelDir —
// there is no safe guess here (verified: two real hosts once genuinely
// disagreed on the path), so a public service on any host is refused by
// planService while this is unset, rather than getting a written-to-nowhere
// config.yml.
func (d Defaults) ResolvedTunnelDir() (dir string, ok bool) {
	if d.TunnelDir == "" {
		return "", false
	}
	return d.TunnelDir, true
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
	// serialize as their string form, or the {mode, groups, bypass_paths}
	// mapping when either carries data. Legacy `auth: true` is accepted on load
	// and re-emitted as `auth: forward`.
	Auth Auth `yaml:"auth,omitempty"`
	// Tunnel opts the service into the public horizon: true = a cloudflared
	// tunnel ingress entry is generated for its fqdn, so the internet can reach
	// it; absent = local only. Every service is local either way — directly via
	// Pi-hole, or by hairpin through the tunnel — so locality is never in
	// question and has no value here (§12).
	//
	// Named for the MECHANISM (it is exactly "does this get a tunnel route",
	// pairing with defaults.tunnel_dir) rather than the older `public`, which
	// invited reading it as "not local" and sat one word away from auth
	// concepts that are about a request's ORIGIN rather than reachability. The
	// legacy key is still accepted on load and re-emitted as `tunnel`.
	//
	// A plain bool, because this is an OPT-IN and not a tri-state: absent and
	// false mean the same thing. `doctor` uses it in one direction only — a
	// service that says it should be reachable is checked against whether it
	// actually is. Exposure that was never opted into is not reported, so
	// adopting the field costs an existing repo nothing.
	Tunnel bool `yaml:"tunnel,omitempty"`
}

// serviceWire is the on-disk form of Service, carrying the legacy keys that are
// accepted on load and migrated away. Service itself holds only current fields,
// so marshalling can never re-emit a legacy key.
type serviceWire struct {
	FQDN     string `yaml:"fqdn"`
	Host     string `yaml:"host"`
	Backend  string `yaml:"backend"`
	Disabled bool   `yaml:"disabled,omitempty"`
	Auth     Auth   `yaml:"auth,omitempty"`
	Tunnel   bool   `yaml:"tunnel,omitempty"`
	// Public is the pre-rename name of Tunnel. Accepted on load, re-emitted as
	// `tunnel`.
	Public bool `yaml:"public,omitempty"`
	// PublicPaths is the pre-move name of Auth.BypassPaths, when auth-exempt
	// paths sat at the top level. Accepted on load, re-emitted under `auth`.
	PublicPaths []string `yaml:"public_paths,omitempty"`
}

// UnmarshalYAML decodes a service and migrates legacy top-level `public_paths`
// into `auth.bypass_paths` (design §3). A legacy `public: false` (from the brief
// tri-state era) decodes to the zero value and omitempty drops the key on the
// next rewrite — false and absent are the same statement now. The migration is one-way and silent:
// the next mutation rewrites services.yaml wholesale, so the old key simply
// stops existing — the same accept-both-then-re-emit approach as the manifest
// and Caddyfile-import renames. An explicit `auth.bypass_paths` wins if a file
// somehow carries both.
func (s *Service) UnmarshalYAML(value *yaml.Node) error {
	var w serviceWire
	if err := value.Decode(&w); err != nil {
		return err
	}
	*s = Service{
		FQDN:     w.FQDN,
		Host:     w.Host,
		Backend:  w.Backend,
		Disabled: w.Disabled,
		Auth:     w.Auth,
		Tunnel:   w.Tunnel,
	}
	if len(w.PublicPaths) > 0 && len(s.Auth.BypassPaths) == 0 {
		s.Auth.BypassPaths = w.PublicPaths
	}
	// Pre-rename `public:` means what `tunnel:` means now. An explicit
	// `tunnel:` wins if a file somehow carries both.
	if w.Public && !s.Tunnel {
		s.Tunnel = true
	}
	return nil
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
	return Parse(data, path)
}

// Parse reads services.yaml text (path is only used to name it in errors and to remember where it lives).
// It is Load without the file read, so a front end can check text BEFORE writing it.
func Parse(data []byte, path string) (*Config, error) {
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
// (design §9). The temp file — and so every file this ever writes — is
// chmod'd to 0644 before the rename: os.CreateTemp defaults to 0600, which
// is invisible for most generated files (Caddy/pihole containers read them
// as root) but silently breaks any file read by a container running as a
// DIFFERENT non-root UID than whatever user runs hemma — cloudflared's
// config.yml, read by its distroless image's UID 65532, hit this twice in
// production: hemma (running as the host user) wrote it 0600, and the
// container could not read its own config, crash-looping until noticed and
// chmod'd by hand on each host.
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
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
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
