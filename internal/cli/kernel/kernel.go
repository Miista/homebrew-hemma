// Package kernel holds the implementation of every hemma command. Each command
// parses its own flags, does its work and prints its own output and errors —
// the CLI (package cli) only routes argv to these entry points. add/update/
// remove mutate the YAML then call the shared sync engine; they contain no
// file-writing logic of their own (design §6 single-writer invariant).
package kernel

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
	"hemma/internal/render"
	syncpkg "hemma/internal/sync"
)

const (
	configName   = "services.yaml"
	manifestName = "hemma-manifest.yaml"
)

// legacyManifestNames are the pre-rename manifest filenames, newest first
// (splitdns era, then sd era); migrated on first load.
var legacyManifestNames = []string{"splitdns-manifest.yaml", "sd-manifest.yaml"}

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
func errf(env *Env, format string, a ...any) {
	fmt.Fprintf(env.Err, "Error: "+format+"\n", a...)
}

// hint prints an indented follow-up line to stderr (next-step guidance),
// after a blank separator line is emitted by the caller.
func hint(env *Env, format string, a ...any) {
	fmt.Fprintf(env.Err, format+"\n", a...)
}

// plural returns singular when n == 1, else singular+"s". Avoids the clumsy
// "flag(s)" hedge in messages.
func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

// The kernel's entry points: one per top-level hemma command. Each takes the
// repo root and/or config path plus the command's own args (everything after
// the command word), parses its own flags, prints its own output, and returns
// the process exit code (design §8).

// Route runs the kernel command named cmd with its args (everything after the
// command word). ok is false when cmd is not a kernel command, so the caller
// can handle its own (help, version, completion) or report an unknown one.
func Route(env *Env, repoRoot, cfgPath, cmd string, args []string) (code int, ok bool) {
	switch cmd {
	case "add":
		return Add(env, repoRoot, cfgPath, args), true
	case "update":
		return Update(env, repoRoot, cfgPath, args), true
	case "remove":
		return Remove(env, repoRoot, cfgPath, args), true
	case "enable":
		return Enable(env, repoRoot, cfgPath, args), true
	case "disable":
		return Disable(env, repoRoot, cfgPath, args), true
	case "rename":
		return Rename(env, repoRoot, cfgPath, args), true
	case "defaults":
		return Defaults(env, cfgPath, args), true
	case "create":
		return Create(env, cfgPath, args), true
	case "list":
		return List(env, cfgPath, args), true
	case "verify":
		return Verify(env, cfgPath, args), true
	case "doctor":
		return Doctor(env, cfgPath, args), true
	case "apply":
		return Apply(env, repoRoot, cfgPath, args), true
	case "deploy":
		return Deploy(env, repoRoot, cfgPath, args), true
	case "measure":
		return Measure(env, cfgPath, args), true
	}
	return 0, false
}

// Add, Update, Remove, Enable, Disable and Rename take a noun
// (service|host|domain) as their first arg; see dispatchNoun.
func Add(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "add", args)
}
func Update(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "update", args)
}
func Remove(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "remove", args)
}
func Enable(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "enable", args)
}
func Disable(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "disable", args)
}
func Rename(env *Env, repoRoot, cfgPath string, args []string) int {
	return dispatchNoun(env, repoRoot, cfgPath, "rename", args)
}

// Defaults shows the repo-wide defaults, or sets one (`defaults set <key> <v>`).
func Defaults(env *Env, cfgPath string, args []string) int {
	return dispatchDefaults(env, cfgPath, args)
}

// Create mints credentials (`create app oidc …`, `create user …`).
func Create(env *Env, cfgPath string, args []string) int { return dispatchCreate(env, cfgPath, args) }

func List(env *Env, cfgPath string, args []string) int    { return cmdList(env, cfgPath, args) }
func Verify(env *Env, cfgPath string, args []string) int  { return cmdVerify(env, cfgPath, args) }
func Doctor(env *Env, cfgPath string, args []string) int  { return cmdDoctor(env, cfgPath, args) }
func Measure(env *Env, cfgPath string, args []string) int { return cmdMeasure(env, cfgPath, args) }

func Apply(env *Env, repoRoot, cfgPath string, args []string) int {
	return cmdApply(env, repoRoot, cfgPath, args)
}
func Deploy(env *Env, repoRoot, cfgPath string, args []string) int {
	return cmdDeploy(env, repoRoot, cfgPath, args)
}

// Errf and Hint print in the house error style (see errf, hint) for the CLI
// layer's own messages.
func Errf(env *Env, format string, a ...any) { errf(env, format, a...) }
func Hint(env *Env, format string, a ...any) { hint(env, format, a...) }

// SetKeys lists the `defaults set` keys, for shell completion.
func SetKeys() []string { return setKeyStrings() }

// SetLocalHost swaps the "which host is this machine" lookup, returning a
// restore func. It exists for tests that need a deterministic machine identity.
func SetLocalHost(f func(*config.Config) string) (restore func()) {
	old := localHost
	localHost = f
	return func() { localHost = old }
}

// dispatchNoun routes verb-first commands of the form
// `<verb> <noun> <args...>` (e.g. "add domain x", "remove service y") to the
// matching handler. One word order across the whole CLI avoids the
// add-service vs noun-add confusion.
func dispatchNoun(env *Env, repoRoot, cfgPath, verb string, args []string) int {
	if len(args) < 1 {
		errf(env, "Missing the noun for %q — expected service, host, or domain.", verb)
		hint(env, "Usage: hemma %s service|host|domain ...", verb)
		return 2
	}
	noun, rest := args[0], args[1:]
	switch noun {
	case "service":
		switch verb {
		case "add":
			return cmdAdd(env, repoRoot, cfgPath, rest)
		case "update":
			return cmdUpdate(env, repoRoot, cfgPath, rest)
		case "remove":
			return cmdRemove(env, repoRoot, cfgPath, rest)
		case "enable":
			return cmdEnableDisable(env, repoRoot, cfgPath, rest, false)
		case "disable":
			return cmdEnableDisable(env, repoRoot, cfgPath, rest, true)
		case "rename":
			return cmdRename(env, repoRoot, cfgPath, rest)
		}
	case "host":
		switch verb {
		case "add":
			return hostAdd(env, cfgPath, rest)
		case "update":
			return hostUpdate(env, cfgPath, rest)
		case "remove":
			return hostRemove(env, cfgPath, rest)
		default:
			errf(env, "Cannot %q a host — hosts support add, update, and remove.", verb)
			return 2
		}
	case "domain":
		switch verb {
		case "add":
			return domainAdd(env, cfgPath, rest)
		case "remove":
			return domainRemove(env, cfgPath, rest)
		default:
			errf(env, "Cannot %q a domain — domains support add and remove.", verb)
			return 2
		}
	default:
		errf(env, "Unknown noun %q — expected service, host, or domain.", noun)
		hint(env, "Usage: hemma %s service|host|domain ...", verb)
		return 2
	}
	// Reached only if a verb/noun combo fell through (shouldn't happen).
	errf(env, "Unsupported: %s %s.", verb, noun)
	return 2
}

// dispatchDefaults routes `defaults [set <key> <value>]`. Bare (no args)
// prints every current default value via cmdDefaultsShow; `set <key>
// <value>` routes to the matching setSpecs entry — see setSpecs' doc comment
// for why the latter is table-driven rather than a hand-written switch.
func dispatchDefaults(env *Env, cfgPath string, args []string) int {
	if len(args) == 0 {
		return cmdDefaultsShow(env, cfgPath)
	}
	if args[0] != "set" {
		errf(env, "Unknown 'defaults' subcommand %q — expected 'set', or no subcommand to show current values.", args[0])
		return 2
	}
	args = args[1:]
	keys := setKeyStrings()
	if len(args) < 1 {
		errf(env, "Missing what to set — expected %s.", oxfordOr(keys))
		hint(env, "Usage: "+strings.Join(setUsages(), "  |  "))
		return 2
	}
	for _, s := range setSpecs {
		if string(s.key) == args[0] {
			return s.run(env, cfgPath, args[1:])
		}
	}
	errf(env, "Unknown setting %q — expected %s.", args[0], oxfordOr(keys))
	return 2
}

// cmdDefaultsShow prints every setSpecs key and its current value (or
// "(unset)"), so `hemma defaults` is a complete inventory of the repo-wide
// settings `hemma defaults set` can change — the reason this verb exists
// rather than staying named `set`: every value it touches genuinely is one
// of config.Defaults' fields, so the noun should say so and double as a
// listing, the same way `hemma list` is to services.yaml's services.
func cmdDefaultsShow(env *Env, cfgPath string) int {
	cfg, code := loadExisting(env, cfgPath, "show the defaults in")
	if cfg == nil {
		return code
	}
	wKey := 0
	for _, s := range setSpecs {
		if len(s.key) > wKey {
			wKey = len(s.key)
		}
	}
	for _, s := range setSpecs {
		v := s.get(cfg)
		if v == "" {
			v = "(unset)"
		}
		fmt.Fprintf(env.Out, "  %-*s  %s\n", wKey, s.key, v)
	}
	return 0
}

// oxfordOr joins items as "a, b, or c" (2 items: "a or b"; 1: "a"). Shared by
// dispatchDefaults' messages so a new setSpecs entry is worded consistently
// with no hand-written list to keep in sync.
func oxfordOr(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
	}
}

// setUsages returns each setSpec's one-line usage string, in setSpecs order,
// for dispatchSet's missing-argument hint.
func setUsages() []string {
	out := make([]string, len(setSpecs))
	for i, s := range setSpecs {
		out[i] = s.usage
	}
	return out
}

func cmdAdd(env *Env, repoRoot, cfgPath string, args []string) int {
	// Usage puts <service> before flags; Go's flag pkg stops at the first
	// positional, so split it off first.
	name, args, ok := leadingName(args)
	if !ok {
		errf(env, "Missing the <service> name.")
		hint(env, "Usage: hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
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
	authMode := fs.String("auth-mode", "", "auth mode: forward|oidc|external|none")
	authGroups := fs.String("auth-groups", "", "comma-separated auth provider groups allowed access")
	bypassPaths := fs.String("bypass-paths", "", "comma-separated request paths exempted from the auth gate (forward mode only)")
	tunnel := fs.Bool("tunnel", false, "generate a cloudflared tunnel ingress entry (reachable from the internet)")
	fs.BoolVar(tunnel, "public", false, "pre-rename alias for --tunnel")
	policyLocal := fs.String("auth-policy-local", "", "auth requirement for local-horizon requests: bypass|one_factor|two_factor|deny")
	policyPublic := fs.String("auth-policy-public", "", "auth requirement for public-horizon requests: bypass|one_factor|two_factor|deny")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Zero flags → the interactive editor (TTY-gated; a non-terminal stdin is
	// refused there). Any flag present keeps the non-interactive path.
	given := 0
	fs.Visit(func(*flag.Flag) { given++ })
	if given == 0 {
		return cmdAddInteractive(env, repoRoot, cfgPath, name)
	}
	mode, ok := resolveAuthMode(env, fs, *authFlag, *authMode)
	if !ok {
		return 2
	}
	groups := splitGroups(*authGroups)
	paths := splitGroups(*bypassPaths)
	// Groups only make sense with an auth gate — refuse before persisting.
	if len(groups) > 0 && !mode.UsesProvider() {
		errf(env, "--auth-groups requires a provider-backed auth mode — pass --auth-mode forward or --auth-mode oidc (external is authenticated outside hemma, so groups have nothing to apply to).")
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
		errf(env, "Missing required %s: %s.", plural(len(missing), "flag"), strings.Join(missing, ", "))
		hint(env, "Usage: hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}

	addLocal, ok := resolvePolicyLevel(env, "auth-policy-local", *policyLocal)
	if !ok {
		return 2
	}
	addPublic, ok := resolvePolicyLevel(env, "auth-policy-public", *policyPublic)
	if !ok {
		return 2
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	return persistNewService(env, repoRoot, cfg, name,
		config.Service{FQDN: *fqdn, Host: *host, Backend: *backend,
			Auth: config.Auth{Mode: mode, Groups: groups, BypassPaths: paths,
				Policy: config.AuthPolicy{Local: addLocal, Public: addPublic}},
			Tunnel: *tunnel})
}

// persistNewService is the shared tail of `add service`: both the flags form
// (cmdAdd) and the interactive editor (cmdAddInteractive) funnel through it —
// validate the entry, persist, and run the single sync path (Incremental,
// design §6.1). Zero mutation logic lives in the editor.
func persistNewService(env *Env, repoRoot string, cfg *config.Config, name string, svc config.Service) int {
	if _, exists := cfg.Services[name]; exists {
		errf(env, "Service %q already exists.", name)
		return 1
	}
	for n, s := range cfg.Services {
		if s.FQDN == svc.FQDN {
			errf(env, "The fqdn %q is already used by service %q.", svc.FQDN, n)
			return 1
		}
	}
	// fqdn must fall under a defined domain — catch typos (e.g. .dl for .dk)
	// before persisting, not as a skip at sync time.
	if _, ok := cfg.MatchDomain(svc.FQDN); !ok {
		errf(env, "The fqdn %q matches no defined domain.", svc.FQDN)
		if doms := cfg.DomainNames(); len(doms) > 0 {
			hint(env, "Defined domains: %s. Add one with 'hemma add domain <name>' or fix the fqdn.", strings.Join(doms, ", "))
		} else {
			hint(env, "No domains defined yet — run 'hemma add domain <name>' first.")
		}
		return 1
	}
	// Host must exist too (else it'd persist then skip at sync).
	if _, ok := cfg.Hosts[svc.Host]; !ok {
		errf(env, "Unknown host %q — defined hosts: %s.", svc.Host, strings.Join(sortedKeysOf(cfg.Hosts), ", "))
		return 1
	}
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf(env, "%v", err)
		return 1
	}
	fmt.Fprintf(env.Out, tick+" Added service %q\n", name)
	return runSync(env, repoRoot, cfg, syncpkg.Incremental)
}

// syncBlockedReason returns a human-readable reason a sync cannot run at all
// (a global precondition), or "" if sync may proceed. Per-entry skips are not
// blockers — only repo-wide preconditions are.
func syncBlockedReason(cfg *config.Config) string {
	if len(cfg.Services) > 0 && cfg.Defaults.DNSHost == "" {
		return "no dns_host is set, so DNS records can't be routed. Set the resolver with: hemma defaults set dns-host <name>"
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
			Fix: []string{"hemma defaults set auth-service <name>"},
		})
	}
	if service && !snippet {
		w = append(w, auth.Advisory{
			Headline: "auth is configured but does nothing — the (auth) snippet is an empty no-op",
			Body:     []string{"auth_service is set but auth_snippet is not."},
			Fix:      []string{"hemma defaults set auth-snippet <path>"},
		})
	}
	if service {
		if _, ok := cfg.Services[cfg.Defaults.AuthService]; !ok {
			w = append(w, auth.Advisory{
				Headline: fmt.Sprintf("auth_service %q is not a defined service", cfg.Defaults.AuthService),
				Fix:      []string{"hemma defaults set auth-service <name>  (or add the service first)"},
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
func printAdvisories(env *Env, repoRoot string, advs []auth.Advisory) {
	rel := func(s string) string {
		return strings.ReplaceAll(s, repoRoot+string(filepath.Separator), "")
	}
	for i, a := range advs {
		if i > 0 {
			fmt.Fprintln(env.Out)
		}
		fmt.Fprintf(env.Out, "%s %s\n", warn, rel(a.Headline))
		for _, b := range a.Body {
			fmt.Fprintf(env.Out, "    %s%s%s\n", dimOn, rel(b), dimOff)
		}
		for j, f := range a.Fix {
			if j == 0 {
				fmt.Fprintf(env.Out, "    %sfix:  %s%s\n", dimOn, rel(f), dimOff)
			} else {
				fmt.Fprintf(env.Out, "          %s%s%s\n", dimOn, rel(f), dimOff)
			}
		}
		if a.Then != "" {
			fmt.Fprintf(env.Out, "    %sthen: %s%s\n", dimOn, a.Then, dimOff)
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
			Fix:      []string{"hemma defaults set auth-service <name>"},
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
		if !s.Auth.Mode.UsesProvider() {
			continue
		}
		svcs = append(svcs, auth.Service{Name: name, FQDN: s.FQDN, Mode: string(s.Auth.Mode), Groups: s.Auth.Groups, BypassPaths: s.Auth.BypassPaths})
	}
	return provider.ValidateConfig(cfgPath, svcs)
}

// resolveAuthMode reconciles the two auth flags into a single AuthMode.
// --auth is a back-compat shorthand for --auth-mode forward; --auth-mode takes
// forward|oidc|none. Passing both is allowed only if they agree. An invalid
// --auth-mode value is a usage error (returns ok=false; caller exits 2).
// resolvePolicyLevel validates an --auth-policy-* value BEFORE the YAML is
// touched. An empty string is valid and means "unset" (that horizon falls back
// to the default gate), which is how the update flags clear the field. A typo
// is a usage error rather than something that reaches a generated
// access-control rule, where it would be a silent security statement.
func resolvePolicyLevel(env *Env, flagName, raw string) (config.PolicyLevel, bool) {
	if raw == "" {
		return "", true
	}
	switch config.PolicyLevel(raw) {
	case config.PolicyBypass, config.PolicyOneFactor, config.PolicyTwoFactor, config.PolicyDeny:
		return config.PolicyLevel(raw), true
	}
	errf(env, "%q is not a valid --%s — expected bypass, one_factor, two_factor, or deny.", raw, flagName)
	return "", false
}

func resolveAuthMode(env *Env, fs *flag.FlagSet, auth bool, authMode string) (config.AuthMode, bool) {
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
		case "external":
			mode = config.AuthExternal
		case "none", "":
			mode = config.AuthNone
		default:
			errf(env, "Invalid --auth-mode %q — expected forward, oidc, external, or none.", authMode)
			return "", false
		}
	}
	if authSet {
		shorthand := config.AuthNone
		if auth {
			shorthand = config.AuthForward
		}
		if modeSet && mode != shorthand {
			errf(env, "Conflicting flags: --auth and --auth-mode disagree.")
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

func cmdUpdate(env *Env, repoRoot, cfgPath string, args []string) int {
	name, args, ok := leadingName(args)
	if !ok {
		errf(env, "Missing the <service> name.")
		hint(env, "Usage: hemma update service <name> [--fqdn ...] [--host ...] [--backend ...]")
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
	authMode := fs.String("auth-mode", "", "auth mode: forward|oidc|external|none")
	authGroups := fs.String("auth-groups", "", "comma-separated auth provider groups ('' clears)")
	bypassPaths := fs.String("bypass-paths", "", "comma-separated request paths exempted from the auth gate ('' clears; forward mode only)")
	tunnel := fs.Bool("tunnel", false, "generate a cloudflared tunnel ingress entry ('--tunnel=false' opts back out)")
	fs.BoolVar(tunnel, "public", false, "pre-rename alias for --tunnel")
	policyLocal := fs.String("auth-policy-local", "", "auth requirement for local-horizon requests ('' clears): bypass|one_factor|two_factor|deny")
	policyPublic := fs.String("auth-policy-public", "", "auth requirement for public-horizon requests ('' clears): bypass|one_factor|two_factor|deny")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Zero flags → the interactive editor (TTY-gated; a non-terminal stdin is
	// refused there). Any flag present keeps the non-interactive path.
	changed := 0
	fs.Visit(func(*flag.Flag) { changed++ })
	if changed == 0 {
		return cmdUpdateInteractive(env, repoRoot, cfgPath, name)
	}
	// Validate --auth/--auth-mode up front (usage error before touching YAML).
	newMode, ok := resolveAuthMode(env, fs, *authFlag, *authMode)
	if !ok {
		return 2
	}
	newLocal, ok := resolvePolicyLevel(env, "auth-policy-local", *policyLocal)
	if !ok {
		return 2
	}
	newPublic, ok := resolvePolicyLevel(env, "auth-policy-public", *policyPublic)
	if !ok {
		return 2
	}

	cfg, code := loadExisting(env, cfgPath, "update")
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[name]
	if !exists {
		errf(env, "Service %q does not exist.", name)
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
		case "bypass-paths":
			// Empty string clears the bypass paths.
			svc.Auth.BypassPaths = splitGroups(*bypassPaths)
		case "tunnel", "public":
			// --tunnel=false opts back out; omitempty then drops the key, so
			// false and absent are the same statement on disk.
			svc.Tunnel = *tunnel
		case "auth-policy-local":
			svc.Auth.Policy.Local = newLocal
		case "auth-policy-public":
			svc.Auth.Policy.Public = newPublic
		}
	})
	// Shared tail with the interactive editor: validate-before-persist, save,
	// single sync path.
	return persistUpdatedService(env, repoRoot, cfg, name, svc)
}

func cmdRemove(env *Env, repoRoot, cfgPath string, args []string) int {
	if len(args) < 1 {
		errf(env, "Missing the <service> name.")
		hint(env, "Usage: hemma remove service <name>")
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(env, cfgPath, "remove")
	if cfg == nil {
		return code
	}
	if _, exists := cfg.Services[name]; !exists {
		fmt.Fprintf(env.Out, "Service %q does not exist; nothing to remove.\n", name)
		return 0
	}
	delete(cfg.Services, name)
	if err := cfg.Save(); err != nil {
		errf(env, "%v", err)
		return 1
	}

	mf := loadManifest(env, repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	res, err := eng.RemoveService(name)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	fmt.Fprintf(env.Out, tick+" Removed service %q\n", name)
	if n := len(res.Deleted); n > 0 {
		fmt.Fprintf(env.Out, tick+" Deleted %d generated %s\n", n, plural(n, "file"))
	} else {
		fmt.Fprintln(env.Out, tick+" No generated files to delete")
	}

	// Deletions still need an apply (drop the vhost/record from running daemons).
	printNextSteps(env, cfg, res)
	reportDrift(env, detectDrift(repoRoot, cfg, mf))
	return 0
}

func cmdEnableDisable(env *Env, repoRoot, cfgPath string, args []string, disable bool) int {
	verb := "enable"
	if disable {
		verb = "disable"
	}
	if len(args) < 1 {
		errf(env, "Missing the <service> name.")
		hint(env, "Usage: hemma %s service <name>", verb)
		return 2
	}
	name := args[0]

	cfg, code := loadExisting(env, cfgPath, verb)
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[name]
	if !exists {
		errf(env, "Service %q does not exist.", name)
		return 1
	}
	if disable && svc.Disabled {
		fmt.Fprintf(env.Out, "Service %q is already disabled.\n", name)
		return 0
	}
	if !disable && !svc.Disabled {
		fmt.Fprintf(env.Out, "Service %q is already enabled.\n", name)
		return 0
	}
	svc.Disabled = disable
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf(env, "%v", err)
		return 1
	}

	if disable {
		// Delete generated files immediately — same as remove but the service
		// stays in services.yaml so it can be re-enabled later.
		mf := loadManifest(env, repoRoot, cfg)
		eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
		res, err := eng.RemoveService(name)
		if err != nil {
			errf(env, "%v", err)
			return 1
		}
		fmt.Fprintf(env.Out, tick+" Disabled service %q\n", name)
		if n := len(res.Deleted); n > 0 {
			fmt.Fprintf(env.Out, tick+" Deleted %d generated %s\n", n, plural(n, "file"))
		}
		return 0
	}

	fmt.Fprintf(env.Out, tick+" Enabled service %q\n", name)
	return runSync(env, repoRoot, cfg, syncpkg.Incremental)
}

// cmdRename renames a service's key in services.yaml. Generated filenames
// embed the service name (design: internal/plan), so a rename can't be done
// in place on disk — it deletes the old-named files via the same targeted
// RemoveService primitive cmdEnableDisable(disable) uses, then a plain
// Incremental sync writes the new-named files. Refuses if <new> already
// exists (never silently overwrite another service's entry) or if <old> is
// the configured auth_service (hemma can't rename the docker-compose
// container or any hand-pasted Authelia OIDC client registration that also
// refer to the old name, so that follow-up must stay a manual, deliberate
// step rather than something this command papers over).
func cmdRename(env *Env, repoRoot, cfgPath string, args []string) int {
	if len(args) < 2 {
		errf(env, "Missing <old> and/or <new> service name.")
		hint(env, "Usage: hemma rename service <old> <new>")
		return 2
	}
	oldName, newName := args[0], args[1]

	cfg, code := loadExisting(env, cfgPath, "rename")
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[oldName]
	if !exists {
		errf(env, "Service %q does not exist.", oldName)
		return 1
	}
	if _, collide := cfg.Services[newName]; collide {
		errf(env, "Service %q already exists.", newName)
		return 1
	}
	if cfg.Defaults.AuthService == oldName {
		errf(env, "Service %q is the configured auth_service — rename refused.", oldName)
		hint(env, "Renaming it would leave the docker-compose container name and any hand-pasted")
		hint(env, "Authelia OIDC client registration pointed at the old name. Rename those by hand")
		hint(env, "first, then 'hemma defaults set auth-service "+newName+"', then retry.")
		return 1
	}

	delete(cfg.Services, oldName)
	cfg.Services[newName] = svc
	if err := cfg.Save(); err != nil {
		errf(env, "%v", err)
		return 1
	}

	mf := loadManifest(env, repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	res, err := eng.RemoveService(oldName)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}
	fmt.Fprintf(env.Out, tick+" Renamed service %q to %q\n", oldName, newName)
	if n := len(res.Deleted); n > 0 {
		fmt.Fprintf(env.Out, tick+" Deleted %d old generated %s\n", n, plural(n, "file"))
	}
	return runSync(env, repoRoot, cfg, syncpkg.Incremental)
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
func runSync(env *Env, repoRoot string, cfg *config.Config, mode syncpkg.Mode) int {
	// Pre-flight: refuse before writing when a repo-wide precondition isn't met.
	if reason := syncBlockedReason(cfg); reason != "" {
		fmt.Fprintf(env.Err, cross+" Not synced: %s\n", reason)
		hint(env, "  The change is saved in services.yaml. Run 'hemma doctor --fix' once that's resolved.")
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
		errf(env, "auth_snippet unreadable — keeping the existing generated auth snippet: %v", authErr)
		plan.PinAuthSnippetToDisk(p, repoRoot)
	}

	// Before writing, warn if any output path would be gitignored — those
	// files would generate fine but never commit/deploy (the repo's
	// **/data/** rule swallows them).
	warnIfIgnored(env, repoRoot, p)

	mf := loadManifest(env, repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}

	res, err := eng.Reconcile(p, mode)
	if err != nil {
		errf(env, "%v", err)
		return 1
	}

	synced, total := len(res.Synced), res.Total
	fmt.Fprintf(env.Out, "Synced %d/%d services.\n", synced, total)

	// Surface an incomplete bootstrap so a no-op/partial sync explains itself,
	// rather than leaving the user wondering why nothing happened.
	if cfg.Defaults.DNSHost == "" {
		fmt.Fprintln(env.Out, "Note: no dns_host set — run 'hemma defaults set dns-host <name>' (records can't be routed without it).")
	}
	if len(cfg.Domains) == 0 {
		fmt.Fprintln(env.Out, "Note: no domains defined — run 'hemma add domain <name>' (a service's fqdn must match a domain).")
	}
	printAdvisories(env, repoRoot, authConfigWarnings(repoRoot, cfg))

	{
		// List only services whose files actually changed this run. A no-op
		// sync lists nothing; an add/update lists just the touched service
		// (plus any other service that incidentally changed — which is the
		// truth of what happened).
		for _, name := range changedServices(p, res) {
			fmt.Fprintf(env.Out, "  • %s\n", name)
		}
	}
	disabled, errored := splitSkipped(res.Skipped)
	if len(disabled) > 0 {
		fmt.Fprintf(env.Out, "%d disabled:\n", len(disabled))
		for _, name := range disabled {
			fmt.Fprintf(env.Out, "  • %s\n", name)
		}
	}
	// A tunnel: true service with no tunnel_dir configured still gets its
	// internal DNS/Caddy files (Plan.UnresolvedTunnels — see its doc comment
	// for why this is deliberately NOT a Skipped reason), but it has no
	// working public ingress, which is a real, actionable misconfiguration:
	// surface it and count it toward a non-zero exit, same as any other
	// skip, rather than reporting a clean "Synced" while public reach is
	// silently broken.
	if len(p.UnresolvedTunnels) > 0 {
		fmt.Fprintf(env.Out, "%d publicly unreachable (tunnel_dir not set):\n", len(p.UnresolvedTunnels))
		for _, name := range p.UnresolvedTunnels {
			fmt.Fprintf(env.Out, "  • %s\n", name)
		}
		hint(env, "Run 'hemma defaults set tunnel-dir <dir>' to fix, or 'hemma update service <name> --tunnel=false' if it was not meant to be reachable.")
	}
	// A service asking for a differing local policy with no local_networks
	// configured still generates everything, including its public policy as the
	// catch-all (Plan.UnresolvedLocalPolicy — see its doc comment). Unlike the
	// tunnel case this does NOT force a non-zero exit: the outcome is the local
	// horizon being treated as strictly as the public one, i.e. more auth than
	// asked for, so nothing is exposed and nothing is broken — it is a
	// did-you-mean, not a misconfiguration.
	if len(p.UnresolvedLocalPolicy) > 0 {
		fmt.Fprintf(env.Out, "%d with no local horizon (local_networks not set):\n", len(p.UnresolvedLocalPolicy))
		for _, name := range p.UnresolvedLocalPolicy {
			fmt.Fprintf(env.Out, "  • %s\n", name)
		}
		hint(env, "Set the home network with 'hemma defaults set local-networks <cidr,...>' — until then these services apply their public policy to every request, including from the LAN.")
	}
	// The auth snippet asked for a forward-auth target that could not be
	// resolved, so every host got the empty (auth) stub instead
	// (Plan.UnresolvedAuthTarget). Unlike the local-policy case this fails toward
	// LESS auth — `import auth` still parses and still validates, it just gates
	// nothing — so it is reported loudly and forces a non-zero exit below.
	if p.UnresolvedAuthTarget {
		fmt.Fprintf(env.Out, "%s auth_snippet needs a %s target but defaults.auth_service does not resolve to a service with a backend or fqdn.\n", warn, render.AuthServiceToken)
		hint(env, "Every host got an empty (auth) stub: services that 'import auth' are NOT protected. Set it with 'hemma defaults set auth-service <name>'.")
	}
	if len(errored) > 0 {
		fmt.Fprintf(env.Out, "%d skipped:\n", len(errored))
		for _, name := range sortedSkip(errored) {
			fmt.Fprintf(env.Out, "  • %s: %s\n", name, errored[name])
		}
		return 1
	}
	if len(p.UnresolvedTunnels) > 0 {
		return 1
	}

	printNextSteps(env, cfg, res)

	// Report (but don't fix) any residual drift — chiefly orphaned files, since
	// the incremental reconcile above never deletes. Points the user at
	// 'hemma doctor --fix'. add/update/remove proceed regardless (report-but-proceed).
	reportDrift(env, detectDrift(repoRoot, cfg, mf))

	// An unreadable auth_snippet source is an error even though the sync
	// proceeded around it (keep-last-good above): exit non-zero (design §8).
	// An unresolved forward-auth target is the same class of problem — the files
	// were written, but what they contain leaves `import auth` gating nothing.
	if authErr != nil || p.UnresolvedAuthTarget {
		return 1
	}
	return 0
}

// printNextSteps prints per-host commands to make changed files live. Printed
// when files were created, updated, OR deleted — a deletion (e.g. a removed
// service or host) also needs an apply to drop the vhost/record from the
// running daemons.
func printNextSteps(env *Env, cfg *config.Config, res *syncpkg.Result) {
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

	fmt.Fprintln(env.Out, "\nTo make changes live, run 'hemma apply' on each host:")
	for _, name := range sortedKeysOf(needApply) {
		if name == self {
			fmt.Fprintln(env.Out, "  hemma apply  # here")
		} else {
			fmt.Fprintf(env.Out, "  on %s:  hemma apply\n", name)
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
func loadManifest(env *Env, repoRoot string, cfg *config.Config) *manifest.Manifest {
	mfPath := filepath.Join(repoRoot, manifestName)
	// Migrate a pre-rename manifest so its tracked-file history (the GC
	// authority) survives the sd -> splitdns -> hemma renames. Rebuild would
	// lose knowledge of files whose service was since removed.
	for _, name := range legacyManifestNames {
		legacy := filepath.Join(repoRoot, name)
		if fileExists(legacy) && !fileExists(mfPath) {
			if err := os.Rename(legacy, mfPath); err == nil {
				fmt.Fprintf(env.Err, "Migrated %s -> %s (commit the rename).\n", name, manifestName)
			}
		}
	}
	mf, ok := manifest.Load(mfPath)
	if !ok {
		fmt.Fprintf(env.Err, "Warning: %s is unreadable — rebuilding it from %s.\n", manifestName, configName)
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
func loadExisting(env *Env, cfgPath, command string) (*config.Config, int) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf(env, "%v", err)
		return nil, 1
	}
	if !cfg.Exists {
		errf(env, "No %s in this directory — nothing to %s.", configName, command)
		fmt.Fprintln(env.Err)
		hint(env, "To create your first service:")
		hint(env, "  hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		fmt.Fprintln(env.Err)
		hint(env, "Or run from the repo root, or pass -C <dir>.")
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
