package cli

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"

	"splitdns/internal/config"
)

// cmdList prints the declared inventory — hosts, domains, services. It is plain
// inventory, not a status view: no per-service validity checks. The one thing
// it does flag is the global can't-sync blocker (no dns_host), as a prominent
// warning BEFORE the inventory. Read-only.
//
// By default the Services section is filtered to those running on THIS host
// (matched by local IP); --all shows every service regardless of host.
func cmdList(cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "list")
	if cfg == nil {
		return code
	}

	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	all := fs.Bool("all", false, "show services on every host, not just this one")
	fs.BoolVar(all, "a", false, "alias for --all")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Which host are we on? If we can't tell (IP matches no host), fall back to
	// showing everything rather than an empty list.
	self := localHost(cfg)
	filtered := !*all && self != ""

	// Big fat warning first: without a dns_host, nothing can sync.
	if cfg.Defaults.DNSHost == "" {
		fmt.Println(warn + " No dns_host set — services cannot sync.")
		fmt.Println("   Set the resolver host:  splitdns set dns-host <name>")
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

	// Auth is repo-wide config, not per-host — its own section, only shown when
	// something is configured (keeps a no-auth repo's output clean). The snippet
	// is any Caddy auth directive (forward_auth, basic_auth, …); splitdns is
	// agnostic to its contents, so the section is named for the mechanism.
	if cfg.Defaults.AuthSnippet != "" || cfg.Defaults.AuthService != "" {
		fmt.Printf("\n%s== Auth ==%s\n", boldOn, boldOff)
		if cfg.Defaults.AuthSnippet != "" {
			fmt.Printf("  snippet:  %s\n", cfg.Defaults.AuthSnippet)
		} else {
			fmt.Println("  snippet:  (none — set with 'splitdns set auth-snippet <path>')")
		}
		if cfg.Defaults.AuthService != "" {
			fmt.Printf("  service:  %s\n", cfg.Defaults.AuthService)
		} else {
			fmt.Println("  service:  (none — set with 'splitdns set auth-service <name>')")
		}
	}

	// Gather the services to show, applying the this-host filter unless --all.
	var svcNames []string
	for _, name := range sortedKeysOf(cfg.Services) {
		if filtered && cfg.Services[name].Host != self {
			continue
		}
		svcNames = append(svcNames, name)
	}

	if filtered {
		fmt.Printf("\n%s== Services on %s (%d of %d) ==%s\n", boldOn, self, len(svcNames), len(cfg.Services), boldOff)
	} else {
		fmt.Printf("\n%s== Services (%d) ==%s\n", boldOn, len(svcNames), boldOff)
	}
	printServiceTable(cfg, svcNames)
	if filtered && len(svcNames) < len(cfg.Services) {
		fmt.Printf("  (%d on other hosts hidden — use --all to show)\n", len(cfg.Services)-len(svcNames))
	}

	for _, msg := range authConfigWarnings(cfg) {
		fmt.Printf("%s %s\n", warn, msg)
	}

	repoRoot := filepath.Dir(cfgPath)
	reportDrift(detectDrift(repoRoot, cfg, loadManifest(repoRoot, cfg)))
	return 0
}

// printServiceTable renders the services as an aligned table with an AUTH
// column. Column widths are computed from the data (including headers) so it
// stays aligned regardless of name/fqdn lengths. Disabled services are marked
// in a trailing note column. When nothing is selected, prints a placeholder.
func printServiceTable(cfg *config.Config, svcNames []string) {
	if len(svcNames) == 0 {
		fmt.Println("  (none)")
		return
	}
	type row struct{ name, fqdn, host, backend, auth, note string }
	rows := make([]row, 0, len(svcNames))
	anyAuth := false
	for _, name := range svcNames {
		svc := cfg.Services[name]
		auth := "-"
		if svc.Auth {
			auth = "✓"
			anyAuth = true
		}
		note := ""
		if svc.Disabled {
			note = "[disabled]"
		}
		rows = append(rows, row{name, svc.FQDN, svc.Host, svc.Backend, auth, note})
	}

	// Header + width computation. AUTH holds either "✓" or "-", both width 1.
	hName, hFQDN, hHost, hBack, hAuth := "NAME", "FQDN", "HOST", "BACKEND", "AUTH"
	wName, wFQDN, wHost, wBack := len(hName), len(hFQDN), len(hHost), len(hBack)
	for _, r := range rows {
		wName = max(wName, len(r.name))
		wFQDN = max(wFQDN, len(r.fqdn))
		wHost = max(wHost, len(r.host))
		wBack = max(wBack, len(r.backend))
	}

	fmt.Printf("  %s%-*s  %-*s  %-*s  %-*s  %s%s\n",
		boldOn, wName, hName, wFQDN, hFQDN, wHost, hHost, wBack, hBack, hAuth, boldOff)
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %s",
			wName, r.name, wFQDN, r.fqdn, wHost, r.host, wBack, r.backend, r.auth)
		if r.note != "" {
			line += "  " + r.note
		}
		fmt.Println(line)
	}
	if anyAuth {
		fmt.Println("  (✓ = imports the auth snippet; disable with 'splitdns update service <name> --auth=false')")
	}
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
