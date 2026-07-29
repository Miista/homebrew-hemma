package cli

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"hemma/internal/config"
	syncpkg "hemma/internal/sync"
)

// host/domain/dns-host mutate the YAML then reconcile (Complete mode) so the
// generated files — chiefly the per-(host × domain) TLS snippets and DNS
// records — are regenerated and any orphans GC'd immediately, leaving the repo
// clean (no drift for `hemma apply` to refuse on). The schema key `hosts:` matches
// the `host` noun. Routing of the verb/noun grammar lives in dispatchNoun
// (cli.go); these are the leaf handlers.

func hostAdd(cfgPath string, args []string) int {
	// Two positionals: <name> <ip>. The IP is the one piece of required data
	// and isn't derivable from anything else.
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: hemma add host <name> <ip> [--ssh <dest>]")
		return 2
	}
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		errf("Missing the <ip> for host %q.", args[0])
		hint("Usage: hemma add host <name> <ip> [--ssh <dest>]")
		return 2
	}
	name, ip := args[0], args[1]
	fs := flag.NewFlagSet("add host", flag.ContinueOnError)
	sshDest := fs.String("ssh", "", "ssh(1) destination for 'hemma deploy' (defaults to the host name)")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}

	if net.ParseIP(ip) == nil {
		errf("%q is not a valid IP address.", ip)
		return 2
	}

	// A host's name IS its repo directory (where its compose and config already
	// live). hemma only adds DNS/Caddy artifacts to a real, already-present host,
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
	// SSH is set only when --ssh was passed; empty defaults to the host name
	// (config.Host.SSHDest).
	cfg.Hosts[name] = config.Host{IP: ip, SSH: *sshDest}
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Added host %q (%s).\n", name, ip)
	// Regenerate so the new host gets its per-domain TLS snippets right away,
	// leaving the repo clean (no drift cliff before `hemma apply`). Complete also
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

// hostUpdate changes a host's ip and/or ssh destination — the update-service
// pattern applied to hosts: validate before persisting, only explicitly
// passed flags change anything.
//
// Sync mode by mutation shape (matching the other host mutations): a changed
// ip rewrites every DNS record pointing at the host, so it syncs Complete
// (same as the other host mutations, which can orphan/rewrite generated
// files); an ssh-only change touches nothing generated, so Incremental (a
// no-op reconcile) suffices.
func hostUpdate(cfgPath string, args []string) int {
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <name>.")
		hint("Usage: hemma update host <name> [--ip <ip>] [--ssh <dest>]")
		return 2
	}
	fs := flag.NewFlagSet("update host", flag.ContinueOnError)
	ip := fs.String("ip", "", "new ip address")
	sshDest := fs.String("ssh", "", "new ssh(1) destination ('-' or '' clears it back to the host name)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ipSet, sshSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ip":
			ipSet = true
		case "ssh":
			sshSet = true
		}
	})
	if !ipSet && !sshSet {
		errf("Nothing to update — pass --ip and/or --ssh.")
		hint("Usage: hemma update host <name> [--ip <ip>] [--ssh <dest>]")
		return 2
	}
	// Validate the ip shape BEFORE touching the YAML (validate-before-persist).
	if ipSet && net.ParseIP(*ip) == nil {
		errf("%q is not a valid IP address.", *ip)
		return 2
	}

	cfg, code := loadExisting(cfgPath, "update a host in")
	if cfg == nil {
		return code
	}
	h, exists := cfg.Hosts[name]
	if !exists {
		errf("Host %q does not exist.", name)
		return 1
	}
	mode := syncpkg.Incremental
	if ipSet {
		// A LAN IP identifies exactly one host (same rule as add host).
		for n, other := range cfg.Hosts {
			if n != name && other.IP == *ip {
				errf("IP %s is already used by host %q.", *ip, n)
				return 1
			}
		}
		if h.IP != *ip {
			// The ip feeds every DNS record targeting this host; rewrite them
			// all and GC anything orphaned, leaving the repo clean.
			mode = syncpkg.Complete
		}
		h.IP = *ip
	}
	if sshSet {
		// '-' (or empty) clears the field back to the default (the host name).
		if *sshDest == "-" {
			h.SSH = ""
		} else {
			h.SSH = *sshDest
		}
	}
	cfg.Hosts[name] = h
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf(tick+" Updated host %q\n", name)
	return runSync(filepath.Dir(cfgPath), cfg, mode)
}

func domainAdd(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: hemma add domain <name>")
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
	// leaving the repo clean (no drift cliff before `hemma apply`).
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

// SetKey is one `hemma set <key> ...` setting. A typed constant rather than a
// bare string so a typo'd key at a call site (dispatchSet, a help/completion
// generator, a test) is a compile error instead of a silent runtime miss.
type SetKey string

const (
	SetDNSHost     SetKey = "dns-host"
	SetDeployHost  SetKey = "deploy-host"
	SetAuthSnippet SetKey = "auth-snippet"
	SetAuthService SetKey = "auth-service"
	SetTunnelDir   SetKey = "tunnel-dir"
)

// setSpec is everything dispatchDefaults, the top-level usage summary,
// `hemma defaults` (bare, no args) and the shell completion scripts need for
// one `defaults set` key. It exists so those places (help.go's longer
// per-command prose is covered instead by TestSetSpecs_HelpTopicsCoverEveryKey)
// are generated from ONE list rather than hand-duplicated — the exact class
// of miss that happened when tunnel-dir was added: dispatchSet, help.go, and
// the usage summary were updated, but completion.go's two independently
// hardcoded set_keys lists were not, and nothing caught it until a stale
// shell completion was noticed by hand.
type setSpec struct {
	key SetKey
	// usage is the single-line form shown in the top-level usage summary and
	// in dispatchDefaults' own error hint, e.g. "hemma defaults set
	// tunnel-dir <dir>   (use '-' to clear)".
	usage string
	// summary is the one-line description in the top-level usage listing.
	summary string
	run     func(cfgPath string, args []string) int
	// get returns the CURRENT value for `hemma defaults` (bare) to print, or
	// "" if unset. Every setSpec's key is a repo-wide config.Defaults field
	// today, so this only needs cfg, not a host/service name.
	get func(cfg *config.Config) string
}

// setSpecs is the single source of truth for every `hemma defaults set
// <key>` and for `hemma defaults`'s printed listing. Add a new setting by
// appending one entry here, adding its cmdSet* function, and its help.go
// prose entry — TestSetSpecs_HelpTopicsCoverEveryKey enforces the last part;
// dispatchDefaults, the usage summary, `hemma defaults`, and both completion
// scripts are all generated from this slice, so they cannot independently
// drift again.
var setSpecs = []setSpec{
	{SetDNSHost, "hemma defaults set dns-host <name>",
		"Set the default resolver host for DNS records.", cmdSetDNSHost,
		func(cfg *config.Config) string { return cfg.Defaults.DNSHost }},
	{SetDeployHost, "hemma defaults set deploy-host <name>   (use '-' to clear)",
		"Name the one host 'hemma deploy' may run from ('-' clears); doctor audits deploy readiness only there.", cmdSetDeployHost,
		func(cfg *config.Config) string { return cfg.Defaults.DeployHost }},
	{SetAuthSnippet, "hemma defaults set auth-snippet <path>   (use '-' to clear)",
		"Set the (auth) snippet source ('-' clears). Services opt in with --auth.", cmdSetAuthSnippet,
		func(cfg *config.Config) string { return cfg.Defaults.AuthSnippet }},
	{SetAuthService, "hemma defaults set auth-service <name>   (use '-' to clear)",
		"Name the forward-auth backend service ('-' clears); preserves X-Forwarded-Host.", cmdSetAuthService,
		func(cfg *config.Config) string { return cfg.Defaults.AuthService }},
	{SetTunnelDir, "hemma defaults set tunnel-dir <dir>   (use '-' to clear)",
		"Set where every host's cloudflared config.yml is written ('-' clears; a public: true service becomes publicly unreachable until it's set — its DNS/Caddy config is unaffected).", cmdSetTunnelDir,
		func(cfg *config.Config) string { return cfg.Defaults.TunnelDir }},
}

// setKeyStrings returns every SetKey as a plain string, in setSpecs order —
// for building error messages, completion word lists, and test assertions
// without repeating the enum-to-string cast at every call site.
func setKeyStrings() []string {
	out := make([]string, len(setSpecs))
	for i, s := range setSpecs {
		out[i] = string(s.key)
	}
	return out
}

// cmdSetDNSHost handles `set dns-host <name>` — sets defaults.dns_host, the
// host whose dnsmasq receives address= records unless a service overrides it.
// Without this, a CLI-only bootstrap leaves dns_host unset and sync refuses.
func cmdSetDNSHost(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: hemma set dns-host <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the dns-host in")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Hosts[name]; !exists {
		errf("Host %q does not exist — add it first with: hemma add host %s <ip>", name, name)
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

// cmdSetDeployHost sets (or clears) defaults.deploy_host — the single host
// `hemma deploy` fans out from, and therefore the only host whose
// deploy-readiness doctor audits. Nothing is generated from it, so unlike
// set dns-host there is no re-sync: it only scopes a check.
func cmdSetDeployHost(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <name>.")
		hint("Usage: hemma set deploy-host <name>   (use '-' to clear)")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "set the deploy-host in")
	if cfg == nil {
		return code
	}
	if name == "-" || name == "" {
		cfg.Defaults.DeployHost = ""
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Println("Cleared deploy_host — doctor audits deploy readiness on every host again.")
		return 0
	}
	if _, exists := cfg.Hosts[name]; !exists {
		errf("Host %q does not exist — add it first with: hemma add host %s <ip>", name, name)
		return 1
	}
	cfg.Defaults.DeployHost = name
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set deploy_host to %q — only that host audits deploy readiness.\n", name)
	return 0
}

// cmdSetAuthSnippet sets (or clears) defaults.auth_snippet — the repo-relative
// path to the Caddy file whose contents become the body of the (auth) snippet
// on every host. Pass an empty path (or "-") to clear it, which regenerates the
// empty (auth) {} stub everywhere (services stay valid but unprotected).
func cmdSetAuthSnippet(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <path>.")
		hint("Usage: hemma set auth-snippet <path>   (use '-' to clear)")
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
		hint("Usage: hemma set auth-service <name>   (use '-' to clear)")
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
		errf("Service %q does not exist — add it first with: hemma add service %s ...", name, name)
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

// cmdSetTunnelDir sets (or clears) defaults.tunnel_dir — the ONE repo-wide
// directory (relative to each host's own repo dir) where hemma writes that
// host's cloudflared config.yml. A single value, not per-host: kept as one
// setting like dns-host/auth-service on the basis that every host's
// cloudflared container mounts the same path in this convention — verified
// against two real hosts that were briefly misaligned and reconciled to
// match, rather than carrying a permanent per-host override for a
// divergence that turned out to be incidental, not structural.
//
// Unlike dns-host, clearing this is NOT a no-op fallback to a hardcoded
// default: any public: true service becomes publicly unreachable while
// tunnel_dir is unset (plan.Plan.UnresolvedTunnels) — but its DNS record and
// Caddy site (the internal horizon) are unaffected, deliberately, since a
// missing tunnel_dir says nothing about whether the service itself is valid.
func cmdSetTunnelDir(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <dir>.")
		hint("Usage: hemma set tunnel-dir <dir>   (use '-' to clear)")
		return 2
	}
	dir := args[0]

	cfg, code := loadExisting(cfgPath, "set the tunnel-dir in")
	if cfg == nil {
		return code
	}
	repoRoot := filepath.Dir(cfgPath)
	if dir == "-" || dir == "" {
		cfg.Defaults.TunnelDir = ""
		if err := cfg.Save(); err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Println("Cleared tunnel_dir — any public: true service becomes publicly unreachable until it's set again (its DNS/Caddy config is unaffected).")
		// The path is now unset, so every host's config.yml becomes an orphan.
		return runSync(repoRoot, cfg, syncpkg.Complete)
	}
	cfg.Defaults.TunnelDir = dir
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("Set tunnel_dir to %q.\n", dir)
	return runSync(repoRoot, cfg, syncpkg.Complete)
}
