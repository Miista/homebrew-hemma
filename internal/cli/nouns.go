package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"sd/internal/config"
)

// host/domain mutate the YAML but do NOT write generated files;
// the schema key `hosts:` matches the `host` noun. A run that changes them is
// followed by sync to regenerate affected services. Routing of the verb/noun
// grammar lives in dispatchNoun (cli.go); these are the leaf handlers.

func hostAdd(cfgPath string, args []string) int {
	// Two positionals: <name> <ip>. The IP is the one piece of required data
	// and isn't derivable from anything else.
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: sd add host <name> <ip>")
		return 2
	}
	if len(args) < 2 {
		errf("Missing the <ip> for host %q.", args[0])
		hint("Usage: sd add host <name> <ip>")
		return 2
	}
	name, ip := args[0], args[1]

	if net.ParseIP(ip) == nil {
		errf("%q is not a valid IP address.", ip)
		return 2
	}

	// A host's name IS its repo directory (where its compose and config already
	// live). sd only adds DNS/Caddy artifacts to a real, already-present host,
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
	return 0
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
	return 0
}

func domainAdd(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: sd add domain <name>")
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
	return 0
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
	return 0
}

// cmdSetDNSHost handles `set dns-host <name>` — sets defaults.dns_host, the
// host whose dnsmasq receives address= records unless a service overrides it.
// Without this, a CLI-only bootstrap leaves dns_host unset and sync refuses.
func cmdSetDNSHost(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: sd set dns-host <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the dns-host in")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Hosts[name]; !exists {
		errf("Host %q does not exist — add it first with: sd add host %s <ip>", name, name)
		return 1
	}
	cfg.Defaults.DNSHost = name
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set default dns_host to %q.\n", name)
	return 0
}
