// Package plan validates each service independently and produces the desired
// set of (path, content) files, collecting per-entry errors rather than
// stopping (design §7).
package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/render"
)

// File is one desired output file.
type File struct {
	Path    string
	Content string
}

// Plan is the result of validating a Config.
type Plan struct {
	// Files keyed by service name -> its desired files.
	Files map[string][]File
	// Skipped service -> reason.
	Skipped map[string]string
	// Total number of services considered.
	Total int
}

// IsDisabled reports whether a skipped service was explicitly disabled (as
// opposed to failing validation).
func IsDisabled(reason string) bool { return reason == "disabled" }

// Valid returns service names that produced files (sorted).
func (p *Plan) Valid() []string {
	out := make([]string, 0, len(p.Files))
	for s := range p.Files {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SkippedNames returns skipped service names (sorted).
func (p *Plan) SkippedNames() []string {
	out := make([]string, 0, len(p.Skipped))
	for s := range p.Skipped {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var (
	// name:port shape for backends (design §4.2/§7).
	backendRe = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]+$`)
	// crude fqdn well-formedness.
	fqdnRe = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$`)
)

// Build validates every service and returns the desired plan.
func Build(c *config.Config) *Plan {
	p := &Plan{
		Files:   map[string][]File{},
		Skipped: map[string]string{},
		Total:   len(c.Services),
	}

	hostNames := sortedKeys(c.Hosts)

	// First pass: detect fqdn collisions and filename collisions across the
	// whole set so both sides of a conflict can be reported (design §7).
	fqdnOwners := map[string][]string{}
	pathOwners := map[string][]string{}
	tentative := map[string][]File{} // svc -> files, before collision pruning

	for name, svc := range c.Services {
		if svc.Disabled {
			p.Skipped[name] = "disabled"
			continue
		}
		files, reason := planService(c, name, svc, hostNames)
		if reason != "" {
			p.Skipped[name] = reason
			continue
		}
		fqdnOwners[svc.FQDN] = append(fqdnOwners[svc.FQDN], name)
		for _, f := range files {
			pathOwners[f.Path] = append(pathOwners[f.Path], name)
		}
		tentative[name] = files
	}

	// Mark fqdn collisions.
	for fqdn, owners := range fqdnOwners {
		if len(owners) > 1 {
			sort.Strings(owners)
			for _, o := range owners {
				p.Skipped[o] = fmt.Sprintf("fqdn collision on %q with %s", fqdn, joinOthers(owners, o))
				delete(tentative, o)
			}
		}
	}
	// Mark filename collisions among survivors.
	for path, owners := range pathOwners {
		live := filterLive(owners, tentative)
		if len(live) > 1 {
			sort.Strings(live)
			for _, o := range live {
				p.Skipped[o] = fmt.Sprintf("output path collision on %q with %s", path, joinOthers(live, o))
				delete(tentative, o)
			}
		}
	}

	for name, files := range tentative {
		p.Files[name] = files
	}

	// TLS snippets are generated independently of services: every managed
	// domain gets its (tls_<domain>) snippet on every host, because the acme
	// pipeline already pushes every cert to every host. Each domain owns its
	// snippets under a synthetic manifest key so GC tracks them per domain,
	// not per service.
	for dom := range c.Domains {
		owner := domainOwner(dom)
		var files []File
		for hostName, h := range c.Hosts {
			path := filepath.Join(h.ResolvedDir(hostName), config.DefaultCaddyTLSDir, render.TLSSnippetName(dom)+".caddy")
			files = append(files, File{Path: path, Content: render.TLSSnippet(dom)})
		}
		if len(files) > 0 {
			p.Files[owner] = files
		}
	}

	// hemma.generated.caddy is written to every host's caddy/data/ dir. It
	// contains the three import lines (auth snippet, tls/*, sites/*) the
	// Caddyfile must import. Owned under a
	// synthetic key so GC tracks it independently of services/domains.
	const caddyImportOwner = caddyImportKey
	var importFiles []File
	for hostName, h := range c.Hosts {
		path := filepath.Join(h.ResolvedDir(hostName), config.DefaultCaddyDataDir, render.CaddyImportFilename)
		importFiles = append(importFiles, File{Path: path, Content: render.CaddyImportFile})
	}
	if len(importFiles) > 0 {
		p.Files[caddyImportOwner] = importFiles
	}

	// The auth snippet (hemma.auth.generated.caddy) is written to
	// every host's caddy/data/ dir, imported before the site blocks. It is
	// always present: its body is the configured auth_snippet content, or an
	// empty (auth) {} stub when none is set. Because it is always planned and
	// manifest-tracked, drift detection compares its on-disk content to the
	// (live) source content for free — no special-casing in doctor. Owned under
	// a synthetic key so GC tracks it independently of services/domains.
	var authFiles []File
	authContent := render.AuthSnippet(c.AuthSnippetBody)
	for hostName, h := range c.Hosts {
		path := filepath.Join(h.ResolvedDir(hostName), config.DefaultCaddyDataDir, render.AuthSnippetFilename)
		authFiles = append(authFiles, File{Path: path, Content: authContent})
	}
	if len(authFiles) > 0 {
		p.Files[authSnippetKey] = authFiles
	}

	// The auth provider's generated access-control artifact (design §4.6) —
	// provider-owned content on the auth_service's host, synthetic-owner
	// tracked like the @domain: TLS snippets. Emitted only when it has
	// something to say (provider decides); otherwise absent and GC'd.
	planAccessControl(c, p)

	return p
}

// planAccessControl asks the auth provider for its access-control artifact
// (path relative to the auth host's dir + content) covering the surviving
// auth-enabled services, and adds it under the @auth-access synthetic owner.
// No-op when defaults.auth_service is unset/invalid or the provider has no
// forward/oidc services to cover — the half-configured cases are already
// surfaced by the cli auth warnings, not here.
func planAccessControl(c *config.Config, p *Plan) {
	if c.Defaults.AuthService == "" {
		return
	}
	authSvc, ok := c.Services[c.Defaults.AuthService]
	if !ok {
		return
	}
	hostM, ok := c.Hosts[authSvc.Host]
	if !ok {
		return
	}
	var svcs []auth.Service
	for name := range p.Files {
		if IsSyntheticOwner(name) {
			continue
		}
		svc, ok := c.Services[name]
		if !ok || svc.Auth.Mode == config.AuthNone {
			continue
		}
		svcs = append(svcs, auth.Service{
			Name:        name,
			FQDN:        svc.FQDN,
			Mode:        string(svc.Auth.Mode),
			Groups:      svc.Auth.Groups,
			BypassPaths: svc.Auth.BypassPaths,
		})
	}
	relPath, content, ok := auth.Default().AccessControl(svcs)
	if !ok {
		return
	}
	path := filepath.Join(hostM.ResolvedDir(authSvc.Host), relPath)
	p.Files[authAccessKey] = []File{{Path: path, Content: content}}
}

// planService validates one entry and returns its files or a skip reason.
func planService(c *config.Config, name string, svc config.Service, hostNames []string) ([]File, string) {
	// fqdn well-formed
	if !fqdnRe.MatchString(svc.FQDN) {
		return nil, fmt.Sprintf("malformed fqdn %q", svc.FQDN)
	}
	// fqdn must match a managed domain; the tls snippet name is derived from it.
	domain, ok := matchDomain(c, svc.FQDN)
	if !ok {
		return nil, fmt.Sprintf("fqdn %q matches no domain in %v", svc.FQDN, sortedKeys(c.Domains))
	}
	tlsImport := render.TLSSnippetName(domain)
	// host host
	hostM, ok := c.Hosts[svc.Host]
	if !ok {
		return nil, fmt.Sprintf("unknown host %q — defined hosts: %s", svc.Host, strings.Join(hostNames, ", "))
	}
	if hostM.IP == "" {
		return nil, fmt.Sprintf("host %q has no ip", svc.Host)
	}
	// The single resolver host (defaults.dns_host) receives every DNS record.
	dnsHostName := c.DNSHost()
	if dnsHostName == "" {
		return nil, "no dns_host set — run 'hemma set dns-host <name>'"
	}
	dnsM, ok := c.Hosts[dnsHostName]
	if !ok {
		return nil, fmt.Sprintf("unknown dns_host %q — defined hosts: %s", dnsHostName, strings.Join(hostNames, ", "))
	}
	// backend shape
	if !backendRe.MatchString(svc.Backend) {
		return nil, fmt.Sprintf("backend %q is not name:port shape", svc.Backend)
	}
	// Loop guard: the service that IS the forward-auth backend (defaults.
	// auth_service, e.g. an Authelia portal) must not also be protected by any
	// auth mode, or every auth subrequest would recurse through the portal.
	// Applies to both forward and oidc — the backend must be reachable un-gated.
	if svc.Auth.Mode != config.AuthNone && name == c.Defaults.AuthService {
		return nil, fmt.Sprintf("auth refused: %q is the auth_service (the forward-auth backend) — protecting it would create a redirect loop", name)
	}
	// Groups grant access via the auth provider's generated rules; with mode
	// none there is no gate for them to apply to — a half-formed intent.
	if svc.Auth.Mode == config.AuthNone && len(svc.Auth.Groups) > 0 {
		return nil, "auth groups set but auth mode is none — set an auth mode (forward|oidc) or clear the groups"
	}

	dnsPath := filepath.Join(dnsM.ResolvedDir(dnsHostName), config.DefaultDnsmasqDir, name+".generated.conf")
	caddyPath := filepath.Join(hostM.ResolvedDir(svc.Host), config.DefaultCaddySitesDir, name+".caddy")

	return []File{
		{Path: dnsPath, Content: render.DNSRecord(svc.FQDN, hostM.IP)},
		{Path: caddyPath, Content: render.CaddySite(svc.FQDN, tlsImport, svc.Backend, svc.Auth.Mode, name == c.Defaults.AuthService)},
	}, ""
}

// domainOwnerPrefix marks synthetic manifest/plan keys that own per-domain TLS
// snippets (as opposed to real service entries).
const domainOwnerPrefix = "@domain:"

// caddyImportKey is the synthetic plan/manifest key for the per-host
// hemma.generated.caddy import file.
const caddyImportKey = "@caddy-import"

// authSnippetKey is the synthetic plan/manifest key for the per-host
// hemma.auth.generated.caddy auth snippet file.
const authSnippetKey = "@auth-snippet"

// authAccessKey is the synthetic plan/manifest key for the auth provider's
// generated access-control artifact on the auth_service's host (design §4.6).
const authAccessKey = "@auth-access"

// IsSyntheticOwner reports whether a plan/manifest key is synthetic (not a
// real service name). Covers domain TLS owners, the caddy-import owner, the
// auth-snippet owner, and the auth access-control owner.
func IsSyntheticOwner(key string) bool {
	return IsDomainOwner(key) || key == caddyImportKey || key == authSnippetKey || key == authAccessKey
}

// PinAuthSnippetToDisk rewrites the planned content of every auth-snippet file
// to whatever is currently on disk, so a sync writes it back unchanged. Used
// when the configured auth_snippet source is unreadable: the file stays planned
// and manifest-tracked (GC-safe) but is NOT reset to the empty stub, preserving
// the last-good auth snippet rather than silently disabling auth. Files
// not yet on disk keep their planned (stub) content — nothing to preserve.
func PinAuthSnippetToDisk(p *Plan, repoRoot string) {
	files := p.Files[authSnippetKey]
	for i, f := range files {
		if b, err := os.ReadFile(filepath.Join(repoRoot, f.Path)); err == nil {
			files[i].Content = string(b)
		}
	}
}

// domainOwner returns the synthetic owner key for a domain's TLS snippets.
func domainOwner(domain string) string { return domainOwnerPrefix + domain }

// IsDomainOwner reports whether a plan/manifest key is a synthetic TLS-snippet
// owner rather than a service name.
func IsDomainOwner(key string) bool { return strings.HasPrefix(key, domainOwnerPrefix) }

// DomainOf returns the domain name from a synthetic owner key (inverse of
// domainOwner); returns the key unchanged if it isn't an owner key.
func DomainOf(key string) string { return strings.TrimPrefix(key, domainOwnerPrefix) }

// matchDomain returns the longest registrable domain suffix of fqdn present in
// the domains map.
func matchDomain(c *config.Config, fqdn string) (string, bool) {
	var bestKey string
	for dom := range c.Domains {
		if fqdn == dom || strings.HasSuffix(fqdn, "."+dom) {
			if len(dom) > len(bestKey) {
				bestKey = dom
			}
		}
	}
	if bestKey == "" {
		return "", false
	}
	return bestKey, true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinOthers(all []string, self string) string {
	others := make([]string, 0, len(all)-1)
	for _, x := range all {
		if x != self {
			others = append(others, x)
		}
	}
	return strings.Join(others, ", ")
}

func filterLive(owners []string, live map[string][]File) []string {
	out := make([]string, 0, len(owners))
	seen := map[string]bool{}
	for _, o := range owners {
		if _, ok := live[o]; ok && !seen[o] {
			out = append(out, o)
			seen[o] = true
		}
	}
	return out
}
