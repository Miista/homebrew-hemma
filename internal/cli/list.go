package cli

import (
	"fmt"
	"sort"

	"shd/internal/plan"
)

// cmdList prints the current declared state — hosts, domains, services — with
// per-service validity. Read-only; writes nothing.
func cmdList(cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "list")
	if cfg == nil {
		return code
	}
	p := plan.Build(cfg)

	// Hosts.
	fmt.Printf("Hosts (%d):\n", len(cfg.Hosts))
	for _, name := range sortedKeysOf(cfg.Hosts) {
		marker := ""
		if name == cfg.Defaults.DNSHost {
			marker = "  (default dns_host)"
		}
		fmt.Printf("  %-12s %s%s\n", name, cfg.Hosts[name].IP, marker)
	}
	if cfg.Defaults.DNSHost == "" {
		fmt.Println("  (no default dns_host set — run: shd dns-host set <name>)")
	}

	// Domains.
	fmt.Printf("\nDomains (%d):\n", len(cfg.Domains))
	for _, name := range sortedKeysOf(cfg.Domains) {
		fmt.Printf("  %s\n", name)
	}

	// Services, with validity.
	fmt.Printf("\nServices (%d):\n", len(cfg.Services))
	for _, name := range sortedKeysOf(cfg.Services) {
		svc := cfg.Services[name]
		if reason, skipped := p.Skipped[name]; skipped {
			fmt.Printf("  ✗ %-12s %s  — skipped: %s\n", name, svc.FQDN, reason)
		} else {
			fmt.Printf("  ✓ %-12s %s -> %s  (%s)\n", name, svc.FQDN, svc.Host, svc.Backend)
		}
	}

	if len(p.Skipped) > 0 {
		return 1 // some entries are invalid; surface it in the exit code
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
