// Package cli parses commands and wires the engine. add/update/remove mutate
// the YAML then call the shared sync engine; they contain no file-writing
// logic of their own (design §6 single-writer invariant).
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/manifest"
	"hemma/internal/plan"
	syncpkg "hemma/internal/sync"
)

const (
	configName   = "services.yaml"
	manifestName = "hemma-manifest.yaml"
)

// legacyManifestNames are the pre-rename manifest filenames, newest first
// (splitdns era, then sd era); migrated on first load.
var legacyManifestNames = []string{"splitdns-manifest.yaml", "sd-manifest.yaml"}

// Version is the build version, overridden at release time via
// -ldflags "-X hemma/internal/cli.Version=...".
var Version = "dev"

// Status glyphs, colored only when stdout is a terminal (so piped/captured
// output stays plain text). green ✓ / red ✗ / yellow ⚠.
var (
	tick    = "✓"
	cross   = "✗"
	warn    = "⚠"
	boldOn  = ""
	boldOff = ""
	dimOn   = ""
	dimOff  = ""
)

func init() {
	if !colorEnabled() {
		return
	}
	tick = "\033[32m✓\033[0m"
	cross = "\033[31m✗\033[0m"
	warn = "\033[33m⚠\033[0m"
	boldOn = "\033[1m"
	boldOff = "\033[0m"
	dimOn = "\033[2m"
	dimOff = "\033[0m"
}

// colorEnabled reports whether ANSI color should be used: stdout is a terminal
// and NO_COLOR is unset (https://no-color.org).
func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// errf prints a user-facing error to stderr in the house style:
//
//	Error: <Capitalized message>.
//
// Pass the message without a leading "Error:" or trailing newline.
func errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
}

// hint prints an indented follow-up line to stderr (next-step guidance),
// after a blank separator line is emitted by the caller.
func hint(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// plural returns singular when n == 1, else singular+"s". Avoids the clumsy
// "flag(s)" hedge in messages.
func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

// Run executes the CLI. Returns a process exit code (design §8: non-zero if
// any entry was skipped or an error occurred).
func Run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	// Operate on ~/docker by default; -C <dir> overrides it (git-style).
	repoRoot := "~/docker"
	if home, err := os.UserHomeDir(); err == nil {
		repoRoot = filepath.Join(home, "docker")
	}
	if args[0] == "-C" || args[0] == "--chdir" {
		if len(args) < 2 {
			errf("The %s flag requires a directory argument.", args[0])
			return 2
		}
		repoRoot = args[1]
		args = args[2:]
		if len(args) < 1 {
			usage()
			return 2
		}
	}
	cfgPath := filepath.Join(repoRoot, configName)

	cmd := args[0]
	rest := args[1:]

	// -h/--help after any command, and `help <cmd>`, print that command's help.
	if maybeHelp(cmd, rest) {
		return 0
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("hemma", Version)
		return 0
	case "add":
		return dispatchNoun(repoRoot, cfgPath, "add", rest)
	case "update":
		return dispatchNoun(repoRoot, cfgPath, "update", rest)
	case "remove":
		return dispatchNoun(repoRoot, cfgPath, "remove", rest)
	case "enable":
		return dispatchNoun(repoRoot, cfgPath, "enable", rest)
	case "disable":
		return dispatchNoun(repoRoot, cfgPath, "disable", rest)
	case "set":
		return dispatchSet(cfgPath, rest)
	case "create":
		return dispatchCreate(cfgPath, rest)
	case "list":
		return cmdList(cfgPath, rest)
	case "verify":
		return cmdVerify(cfgPath, rest)
	case "doctor":
		return cmdDoctor(cfgPath, rest)
	case "apply":
		return cmdApply(repoRoot, cfgPath, rest)
	case "deploy":
		return cmdDeploy(repoRoot, cfgPath, rest)
	case "measure":
		return cmdMeasure(cfgPath, rest)
	case "completion":
		return cmdCompletion(rest)
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		errf("Unknown command %q.", cmd)
		fmt.Fprintln(os.Stderr)
		usage()
		return 2
	}
}

// dispatchNoun routes verb-first commands of the form
// `<verb> <noun> <args...>` (e.g. "add domain x", "remove service y") to the
// matching handler. One word order across the whole CLI avoids the
// add-service vs noun-add confusion.
func dispatchNoun(repoRoot, cfgPath, verb string, args []string) int {
	if len(args) < 1 {
		errf("Missing the noun for %q — expected service, host, or domain.", verb)
		hint("Usage: hemma %s service|host|domain ...", verb)
		return 2
	}
	noun, rest := args[0], args[1:]
	switch noun {
	case "service":
		switch verb {
		case "add":
			return cmdAdd(repoRoot, cfgPath, rest)
		case "update":
			return cmdUpdate(repoRoot, cfgPath, rest)
		case "remove":
			return cmdRemove(repoRoot, cfgPath, rest)
		case "enable":
			return cmdEnableDisable(repoRoot, cfgPath, rest, false)
		case "disable":
			return cmdEnableDisable(repoRoot, cfgPath, rest, true)
		}
	case "host":
		switch verb {
		case "add":
			return hostAdd(cfgPath, rest)
		case "update":
			return hostUpdate(cfgPath, rest)
		case "remove":
			return hostRemove(cfgPath, rest)
		default:
			errf("Cannot %q a host — hosts support add, update, and remove.", verb)
			return 2
		}
	case "domain":
		switch verb {
		case "add":
			return domainAdd(cfgPath, rest)
		case "remove":
			return domainRemove(cfgPath, rest)
		default:
			errf("Cannot %q a domain — domains support add and remove.", verb)
			return 2
		}
	default:
		errf("Unknown noun %q — expected service, host, or domain.", noun)
		hint("Usage: hemma %s service|host|domain ...", verb)
		return 2
	}
	// Reached only if a verb/noun combo fell through (shouldn't happen).
	errf("Unsupported: %s %s.", verb, noun)
	return 2
}

// dispatchSet routes `set <thing> <args>`: `set dns-host`, `set auth-snippet`, and `set auth-service`.
func dispatchSet(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing what to set — expected dns-host, auth-snippet, or auth-service.")
		hint("Usage: hemma set dns-host <name>  |  hemma set auth-snippet <path>  |  hemma set auth-service <name>")
		return 2
	}
	switch args[0] {
	case "dns-host":
		return cmdSetDNSHost(cfgPath, args[1:])
	case "auth-snippet":
		return cmdSetAuthSnippet(cfgPath, args[1:])
	case "auth-service":
		return cmdSetAuthService(cfgPath, args[1:])
	default:
		errf("Unknown setting %q — expected dns-host, auth-snippet, or auth-service.", args[0])
		return 2
	}
}

func cmdAdd(repoRoot, cfgPath string, args []string) int {
	// Usage puts <service> before flags; Go's flag pkg stops at the first
	// positional, so split it off first.
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <service> name.")
		hint("Usage: hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	fs.StringVar(fqdn, "f", "", "alias for --fqdn")
	host := fs.String("host", "", "host that runs the service")
	fs.StringVar(host, "H", "", "alias for --host")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
	fs.StringVar(backend, "b", "", "alias for --backend")
	authFlag := fs.Bool("auth", false, "shorthand for --auth-mode forward")
	authMode := fs.String("auth-mode", "", "auth mode: forward|oidc|none")
	authGroups := fs.String("auth-groups", "", "comma-separated auth provider groups allowed access")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Zero flags → the interactive editor (TTY-gated; a non-terminal stdin is
	// refused there). Any flag present keeps the non-interactive path.
	given := 0
	fs.Visit(func(*flag.Flag) { given++ })
	if given == 0 {
		return cmdAddInteractive(repoRoot, cfgPath, name)
	}
	mode, ok := resolveAuthMode(fs, *authFlag, *authMode)
	if !ok {
		return 2
	}
	groups := splitGroups(*authGroups)
	// Groups only make sense with an auth gate — refuse before persisting.
	if len(groups) > 0 && mode == config.AuthNone {
		errf("--auth-groups requires an auth mode — pass --auth-mode forward or --auth-mode oidc.")
		return 2
	}
	// Validate required flags BEFORE touching the YAML, so a mistyped command
	// never persists a half-formed service entry.
	var missing []string
	if *fqdn == "" {
		missing = append(missing, "--fqdn")
	}
	if *host == "" {
		missing = append(missing, "--host")
	}
	if *backend == "" {
		missing = append(missing, "--backend")
	}
	if len(missing) > 0 {
		errf("Missing required %s: %s.", plural(len(missing), "flag"), strings.Join(missing, ", "))
		hint("Usage: hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return 1
	}
	return persistNewService(repoRoot, cfg, name,
		config.Service{FQDN: *fqdn, Host: *host, Backend: *backend, Auth: config.Auth{Mode: mode, Groups: groups}})
}

// persistNewService is the shared tail of `add service`: both the flags form
// (cmdAdd) and the interactive editor (cmdAddInteractive) funnel through it —
// validate the entry, persist, and run the single sync path (Incremental,
// design §6.1). Zero mutation logic lives in the editor.
func persistNewService(repoRoot string, cfg *config.Config, name string, svc config.Service) int {
	if _, exists := cfg.Services[name]; exists {
		errf("Service %q already exists.", name)
		return 1
	}
	for n, s := range cfg.Services {
		if s.FQDN == svc.FQDN {
			errf("The fqdn %q is already used by service %q.", svc.FQDN, n)
			return 1
		}
	}
	// fqdn must fall under a defined domain — catch typos (e.g. .dl for .dk)
	// before persisting, not as a skip at sync time.
	if _, ok := cfg.MatchDomain(svc.FQDN); !ok {
		errf("The fqdn %q matches no defined domain.", svc.FQDN)
		if doms := cfg.DomainNames(); len(doms) > 0 {
			hint("Defined domains: %s. Add one with 'hemma add domain <name>' or fix the fqdn.", strings.Join(doms, ", "))
		} else {
			hint("No domains defined yet — run 'hemma add domain <name>' first.")
		}
		return 1
	}
	// Host must exist too (else it'd persist then skip at sync).
	if _, ok := cfg.Hosts[svc.Host]; !ok {
		errf("Unknown host %q — defined hosts: %s.", svc.Host, strings.Join(sortedKeysOf(cfg.Hosts), ", "))
		return 1
	}
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf(tick+" Added service %q\n", name)
	return runSync(repoRoot, cfg, syncpkg.Incremental)
}

// syncBlockedReason returns a human-readable reason a sync cannot run at all
// (a global precondition), or "" if sync may proceed. Per-entry skips are not
// blockers — only repo-wide preconditions are.
func syncBlockedReason(cfg *config.Config) string {
	if len(cfg.Services) > 0 && cfg.Defaults.DNSHost == "" {
		return "no dns_host is set, so DNS records can't be routed. Set the resolver with: hemma set dns-host <name>"
	}
	return ""
}

// authConfigWarnings returns non-fatal advisories about a half-configured
// auth setup. Auth still functions without these, so they
// are warnings, not sync blockers (report-but-proceed):
//   - snippet set but no auth_service: the auth backend's block won't preserve
//     X-Forwarded-Host, so post-login redirects loop back to the portal (the
//     exact bug this pairing exists to prevent).
//   - auth_service set but no snippet: the (auth) block is an empty stub, so the
//     header-preserve is emitted but nothing uses it — pointless, likely a
//     mistake.
//   - auth_service names a service that doesn't exist.
//   - fully configured but no service opted in (auth: true): a gentle note that
//     nothing is actually protected yet.
//   - For each auth: oidc service, hemma verifies (read-only) that an Authelia
//     OIDC client registers a redirect_uri for the service. It does NOT configure
//     OIDC — client registration and app env are out of scope.
//
// repoRoot is needed to locate the Authelia config for the OIDC checks.
func authConfigWarnings(repoRoot string, cfg *config.Config) []auth.Advisory {
	var w []auth.Advisory
	snippet := cfg.Defaults.AuthSnippet != ""
	service := cfg.Defaults.AuthService != ""

	anyForward := false
	anyOIDC := false
	for _, s := range cfg.Services {
		switch s.Auth.Mode {
		case config.AuthForward:
			anyForward = true
		case config.AuthOIDC:
			anyOIDC = true
		}
	}

	if snippet && !service {
		w = append(w, auth.Advisory{
			Headline: "post-login redirects will loop back to the auth portal",
			Body: []string{"auth_snippet is set but auth_service is not, so the auth backend's site block",
				"does not preserve X-Forwarded-Host."},
			Fix: []string{"hemma set auth-service <name>"},
		})
	}
	if service && !snippet {
		w = append(w, auth.Advisory{
			Headline: "auth is configured but does nothing — the (auth) snippet is an empty no-op",
			Body:     []string{"auth_service is set but auth_snippet is not."},
			Fix:      []string{"hemma set auth-snippet <path>"},
		})
	}
	if service {
		if _, ok := cfg.Services[cfg.Defaults.AuthService]; !ok {
			w = append(w, auth.Advisory{
				Headline: fmt.Sprintf("auth_service %q is not a defined service", cfg.Defaults.AuthService),
				Fix:      []string{"hemma set auth-service <name>  (or add the service first)"},
			})
		}
	}
	if snippet && service && !anyForward {
		w = append(w, auth.Advisory{
			Headline: "auth is fully configured but nothing is protected yet",
			Body:     []string{"no service uses forward auth."},
			Fix:      []string{"hemma update service <name> --auth-mode forward"},
		})
	}

	// OIDC client-existence checks (read-only; never writes the Authelia config).
	if anyOIDC {
		w = append(w, oidcClientWarnings(repoRoot, cfg)...)
	}
	return w
}

// printAdvisories renders instructive advisories (design §6.4) in the house
// compiler style: a warn-glyph headline stating the consequence, then dim
// indented body lines (the mechanism) and the fixed fix:/then: mini-grammar.
// A blank line separates consecutive advisories. Message CONTENT comes from
// the producer (usually the auth provider); this renderer owns the styling
// and rewrites absolute paths under repoRoot to repo-relative ones (container
// paths like /config/... are untouched). Dimming follows the same TTY /
// NO_COLOR gate as the glyphs; piped output stays plain.
func printAdvisories(repoRoot string, advs []auth.Advisory) {
	rel := func(s string) string {
		return strings.ReplaceAll(s, repoRoot+string(filepath.Separator), "")
	}
	for i, a := range advs {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s %s\n", warn, rel(a.Headline))
		for _, b := range a.Body {
			fmt.Printf("    %s%s%s\n", dimOn, rel(b), dimOff)
		}
		for j, f := range a.Fix {
			if j == 0 {
				fmt.Printf("    %sfix:  %s%s\n", dimOn, rel(f), dimOff)
			} else {
				fmt.Printf("          %s%s%s\n", dimOn, rel(f), dimOff)
			}
		}
		if a.Then != "" {
			fmt.Printf("    %sthen: %s%s\n", dimOn, a.Then, dimOff)
		}
	}
}

// oidcClientWarnings delegates the read-only OIDC checks to the auth
// provider: client existence per auth: oidc service, and — for services with
// auth groups — that the client references the generated authorization_policy.
// The cli only locates the provider's config file
// (<repoRoot>/<auth_service host dir>/<provider config path>); everything
// Authelia-specific lives behind the auth.Provider interface.
func oidcClientWarnings(repoRoot string, cfg *config.Config) []auth.Advisory {
	if cfg.Defaults.AuthService == "" {
		return []auth.Advisory{{
			Headline: "OIDC clients can't be verified — auth_service is not set",
			Fix:      []string{"hemma set auth-service <name>"},
		}}
	}
	authSvc, ok := cfg.Services[cfg.Defaults.AuthService]
	if !ok {
		// Already flagged as a non-existent auth_service above; can't locate config.
		return nil
	}
	hostM, ok := cfg.Hosts[authSvc.Host]
	if !ok {
		return nil
	}
	provider := auth.Default()
	cfgPath := filepath.Join(repoRoot, hostM.ResolvedDir(authSvc.Host), provider.ConfigPath())
	var svcs []auth.Service
	for name, s := range cfg.Services {
		if s.Auth.Mode == config.AuthNone {
			continue
		}
		svcs = append(svcs, auth.Service{Name: name, FQDN: s.FQDN, Mode: string(s.Auth.Mode), Groups: s.Auth.Groups, PublicPaths: s.PublicPaths})
	}
	return provider.ValidateConfig(cfgPath, svcs)
}

// resolveAuthMode reconciles the two auth flags into a single AuthMode.
// --auth is a back-compat shorthand for --auth-mode forward; --auth-mode takes
// forward|oidc|none. Passing both is allowed only if they agree. An invalid
// --auth-mode value is a usage error (returns ok=false; caller exits 2).
func resolveAuthMode(fs *flag.FlagSet, auth bool, authMode string) (config.AuthMode, bool) {
	authSet, modeSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "auth":
			authSet = true
		case "auth-mode":
			modeSet = true
		}
	})
	var mode config.AuthMode
	if modeSet {
		switch authMode {
		case "forward":
			mode = config.AuthForward
		case "oidc":
			mode = config.AuthOIDC
		case "none", "":
			mode = config.AuthNone
		default:
			errf("Invalid --auth-mode %q — expected forward, oidc, or none.", authMode)
			return "", false
		}
	}
	if authSet {
		shorthand := config.AuthNone
		if auth {
			shorthand = config.AuthForward
		}
		if modeSet && mode != shorthand {
			errf("Conflicting flags: --auth and --auth-mode disagree.")
			return "", false
		}
		mode = shorthand
	}
	return mode, true
}

// splitGroups parses a comma-separated --auth-groups value into a clean slice
// (whitespace trimmed, empties dropped). An empty/blank input returns nil,
// which clears the groups.
func splitGroups(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func cmdUpdate(repoRoot, cfgPath string, args []string) int {
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <service> name.")
		hint("Usage: hemma update service <name> [--fqdn ...] [--host ...] [--backend ...]")
		return 2
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	fs.StringVar(fqdn, "f", "", "alias for --fqdn")
	host := fs.String("host", "", "host that runs the service")
	fs.StringVar(host, "H", "", "alias for --host")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
	fs.StringVar(backend, "b", "", "alias for --backend")
	authFlag := fs.Bool("auth", false, "shorthand for --auth-mode forward (--auth=false clears)")
	authMode := fs.String("auth-mode", "", "auth mode: forward|oidc|none")
	authGroups := fs.String("auth-groups", "", "comma-separated auth provider groups ('' clears)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Zero flags → the interactive editor (TTY-gated; a non-terminal stdin is
	// refused there). Any flag present keeps the non-interactive path.
	changed := 0
	fs.Visit(func(*flag.Flag) { changed++ })
	if changed == 0 {
		return cmdUpdateInteractive(repoRoot, cfgPath, name)
	}
	// Validate --auth/--auth-mode up front (usage error before touching YAML).
	newMode, ok := resolveAuthMode(fs, *authFlag, *authMode)
	if !ok {
		return 2
	}

	cfg, code := loadExisting(cfgPath, "update")
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[name]
	if !exists {
		errf("Service %q does not exist.", name)
		return 1
	}
	// Only override fields that were explicitly set on the command line.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "fqdn":
			svc.FQDN = *fqdn
		case "host":
			svc.Host = *host
		case "backend":
			svc.Backend = *backend
		case "auth", "auth-mode":
			// Both flags resolve to newMode (resolveAuthMode rejects conflicts).
			svc.Auth.Mode = newMode
		case "auth-groups":
			// Empty string clears the groups.
			svc.Auth.Groups = splitGroups(*authGroups)
		}
	})
	// Shared tail with the interactive editor: validate-before-persist, save,
	// single sync path.
	return persistUpdatedService(repoRoot, cfg, name, svc)
}

func cmdRemove(repoRoot, cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <service> name.")
		hint("Usage: hemma remove service <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, "remove")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Services[name]; !exists {
		fmt.Printf("Service %q does not exist; nothing to remove.\n", name)
		return 0
	}
	delete(cfg.Services, name)
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}

	mf := loadManifest(repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	res, err := eng.RemoveService(name)
	if err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf(tick+" Removed service %q\n", name)
	if n := len(res.Deleted); n > 0 {
		fmt.Printf(tick+" Deleted %d generated %s\n", n, plural(n, "file"))
	} else {
		fmt.Println(tick + " No generated files to delete")
	}

	// Deletions still need an apply (drop the vhost/record from running daemons).
	printNextSteps(cfg, res)
	reportDrift(detectDrift(repoRoot, cfg, mf))
	return 0
}

func cmdEnableDisable(repoRoot, cfgPath string, args []string, disable bool) int {
	verb := "enable"
	if disable {
		verb = "disable"
	}
	if len(args) < 1 {
		errf("Missing the <service> name.")
		hint("Usage: hemma %s service <name>", verb)
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(cfgPath, verb)
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[name]
	if !exists {
		errf("Service %q does not exist.", name)
		return 1
	}
	if disable && svc.Disabled {
		fmt.Printf("Service %q is already disabled.\n", name)
		return 0
	}
	if !disable && !svc.Disabled {
		fmt.Printf("Service %q is already enabled.\n", name)
		return 0
	}
	svc.Disabled = disable
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}

	if disable {
		// Delete generated files immediately — same as remove but the service
		// stays in services.yaml so it can be re-enabled later.
		mf := loadManifest(repoRoot, cfg)
		eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
		res, err := eng.RemoveService(name)
		if err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Printf(tick+" Disabled service %q\n", name)
		if n := len(res.Deleted); n > 0 {
			fmt.Printf(tick+" Deleted %d generated %s\n", n, plural(n, "file"))
		}
		return 0
	}

	fmt.Printf(tick+" Enabled service %q\n", name)
	return runSync(repoRoot, cfg, syncpkg.Incremental)
}

// runSync builds the plan, reconciles, reports, and returns an exit code
// (design §8). It is the single sync path, invoked as the tail of every
// mutation rather than reimplemented.
//
// Mode differs by mutation shape: service add/update/enable use Incremental
// (write/update, never delete — they can't orphan anything a plain write
// wouldn't overwrite). remove-service and disable don't come through here at
// all: they use the targeted Engine.RemoveService delete primitive (design
// §6.1). Every host/domain/dns-host/auth-snippet/auth-service mutation uses
// Complete, because they can leave orphaned files (a removed service's
// records, or a host/domain's now-dead cross-product of TLS snippets) that
// must be GC'd so the repo is left clean and `hemma apply` won't refuse on
// drift.
func runSync(repoRoot string, cfg *config.Config, mode syncpkg.Mode) int {
	// Pre-flight: refuse before writing when a repo-wide precondition isn't met.
	if reason := syncBlockedReason(cfg); reason != "" {
		fmt.Fprintf(os.Stderr, cross+" Not synced: %s\n", reason)
		hint("  The change is saved in services.yaml. Run 'hemma doctor --fix' once that's resolved.")
		return 1
	}

	// Read the auth snippet source (if configured). On failure, keep the
	// last-good generated snippet: refuse to regenerate the auth file rather than
	// silently reset it to the empty stub, which would drop auth on every
	// protected service fleet-wide. The rest of the sync proceeds
	// (report-but-proceed), but the command exits non-zero (design §4.5/§8) so
	// a deploy script notices the path typo.
	authErr := cfg.LoadAuthSnippet(repoRoot)

	p := plan.Build(cfg)
	if authErr != nil {
		errf("auth_snippet unreadable — keeping the existing generated auth snippet: %v", authErr)
		plan.PinAuthSnippetToDisk(p, repoRoot)
	}

	// Before writing, warn if any output path would be gitignored — those
	// files would generate fine but never commit/deploy (the repo's
	// **/data/** rule swallows them).
	warnIfIgnored(repoRoot, p)

	mf := loadManifest(repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}

	res, err := eng.Reconcile(p, mode)
	if err != nil {
		errf("%v", err)
		return 1
	}

	synced, total := len(res.Synced), res.Total
	fmt.Printf("Synced %d/%d services.\n", synced, total)

	// Surface an incomplete bootstrap so a no-op/partial sync explains itself,
	// rather than leaving the user wondering why nothing happened.
	if cfg.Defaults.DNSHost == "" {
		fmt.Println("Note: no dns_host set — run 'hemma set dns-host <name>' (records can't be routed without it).")
	}
	if len(cfg.Domains) == 0 {
		fmt.Println("Note: no domains defined — run 'hemma add domain <name>' (a service's fqdn must match a domain).")
	}
	printAdvisories(repoRoot, authConfigWarnings(repoRoot, cfg))

	{
		// List only services whose files actually changed this run. A no-op
		// sync lists nothing; an add/update lists just the touched service
		// (plus any other service that incidentally changed — which is the
		// truth of what happened).
		for _, name := range changedServices(p, res) {
			fmt.Printf("  • %s\n", name)
		}
	}
	disabled, errored := splitSkipped(res.Skipped)
	if len(disabled) > 0 {
		fmt.Printf("%d disabled:\n", len(disabled))
		for _, name := range disabled {
			fmt.Printf("  • %s\n", name)
		}
	}
	if len(errored) > 0 {
		fmt.Printf("%d skipped:\n", len(errored))
		for _, name := range sortedSkip(errored) {
			fmt.Printf("  • %s: %s\n", name, errored[name])
		}
		return 1
	}

	printNextSteps(cfg, res)

	// Report (but don't fix) any residual drift — chiefly orphaned files, since
	// the incremental reconcile above never deletes. Points the user at
	// 'hemma doctor --fix'. add/update/remove proceed regardless (report-but-proceed).
	reportDrift(detectDrift(repoRoot, cfg, mf))

	// An unreadable auth_snippet source is an error even though the sync
	// proceeded around it (keep-last-good above): exit non-zero (design §8).
	if authErr != nil {
		return 1
	}
	return 0
}

// printNextSteps prints per-host commands to make changed files live. Printed
// when files were created, updated, OR deleted — a deletion (e.g. a removed
// service or host) also needs an apply to drop the vhost/record from the
// running daemons.
func printNextSteps(cfg *config.Config, res *syncpkg.Result) {
	changed := append(append([]string{}, res.Created...), res.Updated...)
	changed = append(changed, res.Deleted...)
	if len(changed) == 0 {
		return
	}

	dnsDirty := false
	caddyDirty := map[string]bool{} // host name -> true
	for _, path := range changed {
		if strings.Contains(path, "dnsmasq") {
			dnsDirty = true
		} else if strings.Contains(path, "caddy") {
			// first path segment is the host dir; match against host names
			for name, h := range cfg.Hosts {
				dir := h.ResolvedDir(name)
				if strings.HasPrefix(path, dir+"/") || strings.HasPrefix(path, name+"/") {
					caddyDirty[name] = true
				}
			}
		}
	}

	if !dnsDirty && len(caddyDirty) == 0 {
		return
	}

	self := localHost(cfg)

	// Collect the set of hosts that need `hemma apply` run on them: the DNS host
	// (if its records changed) plus every caddy host whose files changed.
	needApply := map[string]bool{}
	if dnsDirty {
		needApply[cfg.Defaults.DNSHost] = true
	}
	for name := range caddyDirty {
		needApply[name] = true
	}

	fmt.Println("\nTo make changes live, run 'hemma apply' on each host:")
	for _, name := range sortedKeysOf(needApply) {
		if name == self {
			fmt.Println("  hemma apply  # here")
		} else {
			fmt.Printf("  on %s:  hemma apply\n", name)
		}
	}
}

// changedServices returns the names of real services (not @domain: TLS owners)
// whose files were created or updated this run, sorted.
func changedServices(p *plan.Plan, res *syncpkg.Result) []string {
	changed := map[string]bool{}
	for _, path := range append(append([]string{}, res.Created...), res.Updated...) {
		changed[path] = true
	}
	var out []string
	for svc, files := range p.Files {
		if plan.IsSyntheticOwner(svc) {
			continue
		}
		for _, f := range files {
			if changed[f.Path] {
				out = append(out, svc)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// loadManifest loads the manifest, rebuilding it if unparseable (design §5/§7).
func loadManifest(repoRoot string, cfg *config.Config) *manifest.Manifest {
	mfPath := filepath.Join(repoRoot, manifestName)
	// Migrate a pre-rename manifest so its tracked-file history (the GC
	// authority) survives the sd -> splitdns -> hemma renames. Rebuild would
	// lose knowledge of files whose service was since removed.
	for _, name := range legacyManifestNames {
		legacy := filepath.Join(repoRoot, name)
		if fileExists(legacy) && !fileExists(mfPath) {
			if err := os.Rename(legacy, mfPath); err == nil {
				fmt.Fprintf(os.Stderr, "Migrated %s -> %s (commit the rename).\n", name, manifestName)
			}
		}
	}
	mf, ok := manifest.Load(mfPath)
	if !ok {
		fmt.Fprintf(os.Stderr, "Warning: %s is unreadable — rebuilding it from %s.\n", manifestName, configName)
		mf = manifest.Rebuild(mfPath, repoRoot, plan.Build(cfg))
	}
	return mf
}

// fileExists reports whether path exists (any type).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// splitSkipped separates disabled services from validation-errored ones.
func splitSkipped(skipped map[string]string) (disabled []string, errored map[string]string) {
	errored = map[string]string{}
	for name, reason := range skipped {
		if plan.IsDisabled(reason) {
			disabled = append(disabled, name)
		} else {
			errored[name] = reason
		}
	}
	sort.Strings(disabled)
	return
}

func sortedSkip(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort to avoid importing sort here twice; small N
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// loadExisting loads services.yaml for commands that read existing state
// (sync, update, remove). A missing file is treated as user error with a
// guiding message rather than an empty config, so a new user in the wrong
// directory — or one who hasn't created any service yet — is told what to do.
// add does NOT use this: it is allowed to create services.yaml.
func loadExisting(cfgPath, command string) (*config.Config, int) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return nil, 1
	}
	if !cfg.Exists {
		errf("No %s in this directory — nothing to %s.", configName, command)
		fmt.Fprintln(os.Stderr)
		hint("To create your first service:")
		hint("  hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		fmt.Fprintln(os.Stderr)
		hint("Or run from the repo root, or pass -C <dir>.")
		return nil, 1
	}
	return cfg, 0
}

// leadingName splits a leading positional <service> from the remaining flag
// args. Returns ok=false if the first token is missing or looks like a flag.
func leadingName(args []string) (name string, rest []string, ok bool) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return "", args, false
	}
	return args[0], args[1:], true
}

func usage() {
	fmt.Fprint(os.Stderr, UsageText)
}

// UsageText is the top-level help, also compiled into the man page by
// tools/genman.
const UsageText = `hemma — Split-Horizon DNS (Manager)

Generates split-horizon DNS records and Caddy site blocks from a declarative
services.yaml. Operates on ~/docker by default; -C <dir> overrides.

Commands are verb-first: <verb> <noun> <args>.

Services (an app reached at an fqdn, on a host, under a domain):
  hemma add     service <name> --fqdn <f> --host <h> --backend <b> [--auth-mode forward|oidc] [--auth-groups <g1,g2>]
                                 With no flags: an interactive editor (terminal only) collects the fields.
  hemma update  service <name> [--fqdn ...] [--host ...] [--backend ...] [--auth-mode forward|oidc|none] [--auth-groups <g1,g2>]
                                 With no flags: an interactive editor (terminal only), pre-filled with current values.
  hemma remove  service <name>
  hemma disable service <name>   Stop generating DNS/Caddy config for a service (keeps it in services.yaml).
  hemma enable  service <name>   Re-enable a disabled service (regenerates its files).

Building blocks (a service references a host and a domain):
  hemma add    host   <name> <ip> [--ssh <dest>]
  hemma update host   <name> [--ip <ip>] [--ssh <dest>]
  hemma remove host   <name>
  hemma add    domain <name>
  hemma remove domain <name>
  hemma set    dns-host <name>       Set the default resolver host for DNS records.
  hemma set    auth-snippet <path>   Set the (auth) snippet source ('-' clears). Services opt in with --auth.
  hemma set    auth-service <name>   Name the forward-auth backend service ('-' clears); preserves X-Forwarded-Host.

Credentials (print-only; the auth provider's config and users database are never written):
  hemma create app oidc <app_name> [callback_path]   Generate OIDC client credentials + a config snippet to paste in.
  hemma create user <username>                       Interactively hash a new user's password + print the users-database snippet.

Other:
  hemma apply                    Make config live on THIS host: validate caddy + auth config first, then restart pihole / reload caddy / restart the auth provider. Run on each host. Refuses if the repo has drift.
  hemma deploy [<host> ...]      Push-based fan-out over ssh: 'git pull --ff-only' on every target host (any failure aborts everything), then 'hemma apply' per host, remotes first and this host last. Targets default to all hosts with a role. Requires a clean, pushed local repo.
  hemma list [--all]             Overview: hosts, domains, services, and auth groups (users + restricted services). Services default to THIS host; --all shows every host.
  hemma verify [--all] [<fqdn>]  Check live DNS/Caddy per service. Defaults to services this host can check; --all includes the rest. Run on each host; needs docker.
  hemma measure [--compare] [-n <runs>] [-w <warmup>] <service|fqdn|url>  Time the request breakdown (dns/connect/tls/ttfb) for a service or any URL. --compare A/Bs split-horizon vs public read-only (dns-host only, services only).
  hemma doctor [--fix]           Audit the repo (gitignored files, Caddyfile imports, generated-file drift); --fix reconciles files and .gitignore.
  hemma version
  hemma completion <bash|zsh>    Print a shell completion script to stdout (see 'hemma help completion' to install).
  hemma help [<command>]         Show this text, or a command's help (same as <command> --help).

Global flags:
  -C, --chdir <dir>   Operate on <dir> instead of the default ~/docker.

Notes:
  - A host's name is its repo directory (e.g. host "pi" -> ./pi/), which must already exist.
  - Each domain gets a TLS snippet generated on every host, deriving cert paths from
    the convention caddy/data/certs/<domain>/{fullchain.cer,privkey.key}.
  - Config edits (add/update/remove/enable/disable) regenerate files automatically, then
    print which hosts to run 'hemma apply' on to make the change live.
  - Removing a host or domain is refused while any service still references it.
`
