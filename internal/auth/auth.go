// Package auth defines the pluggable auth-provider boundary. Everything that
// is specific to one auth provider's config format (today: Authelia) lives
// behind the Provider interface, so swapping providers (e.g. tinyauth) is a
// new implementation plus a Register call — plan and cli stay
// provider-agnostic and only consume (path, content) pairs and warning
// strings.
package auth

import "sort"

// Mode strings as they appear in services.yaml (mirrors config.AuthMode; kept
// as plain strings here so this package does not depend on config).
const (
	ModeForward = "forward"
	ModeOIDC    = "oidc"
)

// Service is the provider-facing view of one auth-enabled service. It carries
// only what a provider needs to render access-control rules and validate its
// own config — no repo/host layout details.
type Service struct {
	Name        string
	FQDN        string
	Mode        string   // ModeForward or ModeOIDC
	Groups      []string // provider group names allowed access; empty = any authenticated user
	PublicPaths []string // paths exempt from auth (forward mode only)
}

// Provider is one auth provider (Authelia, tinyauth, ...). Implementations own
// two things:
//
//  1. the generated access-control artifact — its output path (relative to the
//     auth_service host's repo directory) and its rendered content, given the
//     set of auth-enabled services; and
//  2. the read-only validation of the provider's own config file.
//
// Paths returned are relative to the auth host's directory; the caller (plan
// / cli) joins them with the repo layout. Providers never write files —
// content flows into the plan and the sync engine remains the single writer.
type Provider interface {
	// Name is the registry key (e.g. "authelia").
	Name() string
	// ConfigPath is the provider's own config file, relative to the auth
	// host's repo directory. hemma reads it read-only for validation; it
	// never writes it.
	ConfigPath() string
	// AccessControl renders the generated access-control artifact for the
	// given auth-enabled services. path is relative to the auth host's repo
	// directory. ok=false means nothing should be emitted (the file, if
	// previously generated, becomes an orphan and is GC'd).
	AccessControl(services []Service) (path, content string, ok bool)
	// ValidateConfig read-only checks the provider config at absolute path
	// cfgPath against the auth-enabled services and returns human-readable
	// warnings (report-but-proceed; never fatal).
	ValidateConfig(cfgPath string, services []Service) []string
	// ApplyCommands returns the commands (argv) `hemma apply` runs on the
	// auth host to make a synced provider config live: validate runs first
	// and must succeed before reload runs (the caddy validate-before-reload
	// pattern). container is the provider's container name (the auth_service
	// name by convention). A nil validate skips straight to reload; a nil
	// reload means the provider needs no apply step at all.
	ApplyCommands(container string) (validate, reload []string)
}

// DefaultName is the provider used when none is selected explicitly.
const DefaultName = "authelia"

var registry = map[string]Provider{}

// Register adds a provider to the compile-time registry (call from init).
func Register(p Provider) { registry[p.Name()] = p }

// Lookup returns the provider registered under name.
func Lookup(name string) (Provider, bool) {
	p, ok := registry[name]
	return p, ok
}

// Default returns the default provider (DefaultName). It panics if the
// default implementation failed to register — a build-time wiring bug.
func Default() Provider {
	p, ok := Lookup(DefaultName)
	if !ok {
		panic("auth: default provider " + DefaultName + " not registered")
	}
	return p
}

// Names returns the registered provider names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
