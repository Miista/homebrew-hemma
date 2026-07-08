package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"splitdns/internal/config"
	syncpkg "splitdns/internal/sync"
)

// host/domain/dns-host mutate the YAML then reconcile (Complete mode) so the
// generated files — chiefly the per-(host × domain) TLS snippets and DNS
// records — are regenerated and any orphans GC'd immediately, leaving the repo
// clean (no drift for `splitdns apply` to refuse on). The schema key `hosts:` matches
// the `host` noun. Routing of the verb/noun grammar lives in dispatchNoun
// (cli.go); these are the leaf handlers.

func hostAdd(cfgPath string, args []string) int {
	// Two positionals: <name> <ip>. The IP is the one piece of required data
	// and isn't derivable from anything else.
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: splitdns add host <name> <ip>")
		return 2
	}
	if len(args) < 2 {
		errf("Missing the <ip> for host %q.", args[0])
		hint("Usage: splitdns add host <name> <ip>")
		return 2
	}
	name, ip := args[0], args[1]

	if net.ParseIP(ip) == nil {
		errf("%q is not a valid IP address.", ip)
		return 2
	}

	// A host's name IS its repo directory (where its compose and config already
	// live). splitdns only adds DNS/Caddy artifacts to a real, already-present host,
	// so a name with no matching directory is a typo — refuse it.
	repoRoot := filepath.Dir(cfgPath)
	if info, err := os.Stat(filepath.Join(repoRoot, name)); err != nil || !info.IsDir() {
		errf("No directory %q in the repo.", name)
		hint("A host's name is its repo directory, which must already exist. Check the name for a typo.")
		return 1
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return 1
	}
	if _, exists := cfg.Hosts[name]; exists {
		errf("Host %q already exists.", name)
		return 1
	}
	// A LAN IP identifies exactly one host; two hosts sharing one is a typo.
	for n, h := range cfg.Hosts {
		if h.IP == ip {
			errf("IP %s is already used by host %q.", ip, n)
			return 1
		}
	}
	// Dir is left empty; it defaults to the host name (config.Host.ResolvedDir).
	cfg.Hosts[name] = config.Host{IP: ip}
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Added host %q (%s).\n", name, ip)
	// Regenerate so the new host gets its per-domain TLS snippets right away,
	// leaving the repo clean (no drift cliff before `splitdns apply`). Complete also
	// GCs, which is a harmless no-op for a pure add.
	return runSync(repoRoot, cfg, syncpkg.Complete)
}

func hostRemove(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "remove a host from")
	if cfg == nil {
		return code
	}
	repoRoot := filepath.Dir(cfgPath)
	if _, exists := cfg.Hosts[name]; !exists {
		fmt.Printf("Host %q does not exist; nothing to remove.\n", name)
		return 0
	}
	if users := cfg.ServicesUsingHost(name); len(users) > 0 {
		errf("Host %q is still referenced by %d %s: %s.", name, len(users), plural(len(users), "service"), strings.Join(users, ", "))
		hint("Reassign or remove those services first.")
		return 1
	}
	delete(cfg.Hosts, name)
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Removed host %q.\n", name)
	// Complete reconcile GCs the removed host's now-orphaned TLS snippets so the
	// repo is left clean.
	return runSync(repoRoot, cfg, syncpkg.Complete)
}

func domainAdd(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: splitdns add domain <name>")
		return 2
	}
	name := args[0]

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return 1
	}
	if _, exists := cfg.Domains[name]; exists {
		errf("Domain %q already exists.", name)
		return 1
	}
	cfg.Domains[name] = config.Domain{}
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Added domain %q.\n", name)
	// Regenerate so the new domain's per-host TLS snippets exist right away,
	// leaving the repo clean (no drift cliff before `splitdns apply`).
	return runSync(filepath.Dir(cfgPath), cfg, syncpkg.Complete)
}

func domainRemove(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "remove a domain from")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Domains[name]; !exists {
		fmt.Printf("Domain %q does not exist; nothing to remove.\n", name)
		return 0
	}
	if users := cfg.ServicesUsingDomain(name); len(users) > 0 {
		errf("Domain %q is still referenced by %d %s: %s.", name, len(users), plural(len(users), "service"), strings.Join(users, ", "))
		hint("Reassign or remove those services first.")
		return 1
	}
	delete(cfg.Domains, name)
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Removed domain %q.\n", name)
	// Complete reconcile GCs the removed domain's TLS snippets across all hosts.
	return runSync(filepath.Dir(cfgPath), cfg, syncpkg.Complete)
}

// cmdSetDNSHost handles `set dns-host <name>` — sets defaults.dns_host, the
// host whose dnsmasq receives address= records unless a service overrides it.
// Without this, a CLI-only bootstrap leaves dns_host unset and sync refuses.
func cmdSetDNSHost(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: splitdns set dns-host <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the dns-host in")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Hosts[name]; !exists {
		errf("Host %q does not exist — add it first with: splitdns add host %s <ip>", name, name)
		return 1
	}
	cfg.Defaults.DNSHost = name
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set default dns_host to %q.\n", name)
	// The resolver changed, so every DNS record regenerates. Complete also GCs
	// records from a previously-set resolver host, leaving the repo clean.
	return runSync(filepath.Dir(cfgPath), cfg, syncpkg.Complete)
}

// cmdSetAuthSnippet sets (or clears) defaults.auth_snippet — the repo-relative
// path to the Caddy file whose contents become the body of the (auth) snippet
// on every host. Pass an empty path (or "-") to clear it, which regenerates the
// empty (auth) {} stub everywhere (services stay valid but unprotected).
func cmdSetAuthSnippet(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <path>.")
		hint("Usage: splitdns set auth-snippet <path>   (use '-' to clear)")
		return 2
	}
	path := args[0]

	cfg, code := loadExisting(cfgPath, "set the auth-snippet in")
	if cfg == nil {
		return code
	}
	repoRoot := filepath.Dir(cfgPath)
	if path == "-" || path == "" {
		cfg.Defaults.AuthSnippet = ""
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Println("Cleared auth_snippet — the generated (auth) snippet is now an empty no-op stub.")
		return runSync(repoRoot, cfg, syncpkg.Complete)
	}
	// Validate the source exists before persisting, so a typo is caught here
	// rather than as a keep-last-good warning at every future sync.
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(repoRoot, abs)
	}
	if _, err := os.Stat(abs); err != nil {
		errf("auth_snippet %q is not readable: %v", path, err)
		return 1
	}
	cfg.Defaults.AuthSnippet = path
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set auth_snippet to %q.\n", path)
	// The snippet content changed for every host, so regenerate all auth files.
	return runSync(repoRoot, cfg, syncpkg.Complete)
}

// cmdSetAuthService names the service that is the forward-auth backend (e.g. an
// Authelia portal). Its site block gains a header_up that preserves the inbound
// X-Forwarded-Host, so post-login redirects target the original service rather
// than looping back to the portal. Parallels set dns-host: names one repo-wide
// role by service name; '-' clears it.
func cmdSetAuthService(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: splitdns set auth-service <name>   (use '-' to clear)")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the auth-service in")
	if cfg == nil {
		return code
	}
	repoRoot := filepath.Dir(cfgPath)
	if name == "-" || name == "" {
		cfg.Defaults.AuthService = ""
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Println("Cleared auth_service — the auth backend's site block no longer preserves X-Forwarded-Host.")
		return runSync(repoRoot, cfg, syncpkg.Complete)
	}
	// The named service must exist, else its block can't be rendered specially
	// (mirrors set dns-host refusing an unknown host).
	if _, exists := cfg.Services[name]; !exists {
		errf("Service %q does not exist — add it first with: splitdns add service %s ...", name, name)
		return 1
	}
	cfg.Defaults.AuthService = name
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set auth_service to %q.\n", name)
	// Its site block changes (gains the header_up), so regenerate.
	return runSync(repoRoot, cfg, syncpkg.Complete)
}
