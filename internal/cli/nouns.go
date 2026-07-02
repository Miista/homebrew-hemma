package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"mirage/internal/config"
	syncpkg "mirage/internal/sync"
)

// host/domain/dns-host mutate the YAML then reconcile (Complete mode) so the
// generated files — chiefly the per-(host × domain) TLS snippets and DNS
// records — are regenerated and any orphans GC'd immediately, leaving the repo
// clean (no drift for `mirage apply` to refuse on). The schema key `hosts:` matches
// the `host` noun. Routing of the verb/noun grammar lives in dispatchNoun
// (cli.go); these are the leaf handlers.

func hostAdd(cfgPath string, args []string) int {
	// Two positionals: <name> <ip>. The IP is the one piece of required data
	// and isn't derivable from anything else.
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: mirage add host <name> <ip>")
		return 2
	}
	if len(args) < 2 {
		errf("Missing the <ip> for host %q.", args[0])
		hint("Usage: mirage add host <name> <ip>")
		return 2
	}
	name, ip := args[0], args[1]

	if net.ParseIP(ip) == nil {
		errf("%q is not a valid IP address.", ip)
		return 2
	}

	// A host's name IS its repo directory (where its compose and config already
	// live). mirage only adds DNS/Caddy artifacts to a real, already-present host,
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
	// leaving the repo clean (no drift cliff before `mirage apply`). Complete also
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
		hint("Usage: mirage add domain <name>")
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
	// leaving the repo clean (no drift cliff before `mirage apply`).
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
		hint("Usage: mirage set dns-host <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the dns-host in")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Hosts[name]; !exists {
		errf("Host %q does not exist — add it first with: mirage add host %s <ip>", name, name)
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
