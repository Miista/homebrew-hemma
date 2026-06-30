package cli

import (
	"fmt"
	"sort"
)

// cmdList prints the declared inventory — hosts, domains, services. It is plain
// inventory, not a status view: no per-service validity checks. The one thing
// it does flag is the global can't-sync blocker (no dns_host), as a prominent
// warning BEFORE the inventory. Read-only.
func cmdList(cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "list")
	if cfg == nil {
		return code
	}

	// Big fat warning first: without a dns_host, nothing can sync.
	if cfg.Defaults.DNSHost == "" {
		fmt.Println(warn + " No dns_host set — services cannot sync.")
		fmt.Println("   Set the resolver host:  sd set dns-host <name>")
		fmt.Println()
	}

	fmt.Printf("%s== Hosts (%d) ==%s\n", boldOn, len(cfg.Hosts), boldOff)
	for _, name := range sortedKeysOf(cfg.Hosts) {
		marker := ""
		if name == cfg.Defaults.DNSHost {
			marker = "  (dns_host)"
		}
		fmt.Printf("  %-12s %s%s\n", name, cfg.Hosts[name].IP, marker)
	}

	fmt.Printf("\n%s== Domains (%d) ==%s\n", boldOn, len(cfg.Domains), boldOff)
	for _, name := range sortedKeysOf(cfg.Domains) {
		fmt.Printf("  %s\n", name)
	}

	fmt.Printf("\n%s== Services (%d) ==%s\n", boldOn, len(cfg.Services), boldOff)
	for _, name := range sortedKeysOf(cfg.Services) {
		svc := cfg.Services[name]
		fmt.Printf("  %-12s %s -> %s  (%s)\n", name, svc.FQDN, svc.Host, svc.Backend)
	}
	return 0
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
