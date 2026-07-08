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

	"splitdns/internal/config"
	"splitdns/internal/manifest"
	"splitdns/internal/plan"
	syncpkg "splitdns/internal/sync"
)

const (
	configName         = "services.yaml"
	manifestName       = "splitdns-manifest.yaml"
	legacyManifestName = "sd-manifest.yaml" // pre-rename; migrated on first load
)

// Version is the build version, overridden at release time via
// -ldflags "-X splitdns/internal/cli.Version=...".
var Version = "dev"

// Status glyphs, colored only when stdout is a terminal (so piped/captured
// output stays plain text). green ✓ / red ✗ / yellow ⚠.
var (
	tick    = "✓"
	cross   = "✗"
	warn    = "⚠"
	boldOn  = ""
	boldOff = ""
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
		fmt.Println("splitdns", Version)
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
	case "list":
		return cmdList(cfgPath, rest)
	case "verify":
		return cmdVerify(cfgPath, rest)
	case "doctor":
		return cmdDoctor(cfgPath, rest)
	case "apply":
		return cmdApply(repoRoot, cfgPath, rest)
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
		hint("Usage: splitdns %s service|host|domain ...", verb)
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
		case "remove":
			return hostRemove(cfgPath, rest)
		default:
			errf("Cannot %q a host — hosts support add and remove.", verb)
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
		hint("Usage: splitdns %s service|host|domain ...", verb)
		return 2
	}
	// Reached only if a verb/noun combo fell through (shouldn't happen).
	errf("Unsupported: %s %s.", verb, noun)
	return 2
}

// dispatchSet routes `set <thing> <args>`: `set dns-host` and `set auth-snippet`.
func dispatchSet(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing what to set — expected dns-host, auth-snippet, or auth-service.")
		hint("Usage: splitdns set dns-host <name>  |  splitdns set auth-snippet <path>  |  splitdns set auth-service <name>")
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
		hint("Usage: splitdns add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	fs.StringVar(fqdn, "f", "", "alias for --fqdn")
	host := fs.String("host", "", "host that runs the service")
	fs.StringVar(host, "H", "", "alias for --host")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
	fs.StringVar(backend, "b", "", "alias for --backend")
	auth := fs.Bool("auth", false, "put this service behind the (auth) snippet")
	if err := fs.Parse(args); err != nil {
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
		hint("Usage: splitdns add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return 1
	}
	if _, exists := cfg.Services[name]; exists {
		errf("Service %q already exists.", name)
		return 1
	}
	for n, s := range cfg.Services {
		if s.FQDN == *fqdn {
			errf("The fqdn %q is already used by service %q.", *fqdn, n)
			return 1
		}
	}
	// fqdn must fall under a defined domain — catch typos (e.g. .dl for .dk)
	// before persisting, not as a skip at sync time.
	if _, ok := cfg.MatchDomain(*fqdn); !ok {
		errf("The fqdn %q matches no defined domain.", *fqdn)
		if doms := cfg.DomainNames(); len(doms) > 0 {
			hint("Defined domains: %s. Add one with 'splitdns add domain <name>' or fix the fqdn.", strings.Join(doms, ", "))
		} else {
			hint("No domains defined yet — run 'splitdns add domain <name>' first.")
		}
		return 1
	}
	// Host must exist too (else it'd persist then skip at sync).
	if _, ok := cfg.Hosts[*host]; !ok {
		errf("Unknown host %q — defined hosts: %s.", *host, strings.Join(sortedKeysOf(cfg.Hosts), ", "))
		return 1
	}
	cfg.Services[name] = config.Service{FQDN: *fqdn, Host: *host, Backend: *backend, Auth: *auth}
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
		return "no dns_host is set, so DNS records can't be routed. Set the resolver with: splitdns set dns-host <name>"
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
func authConfigWarnings(cfg *config.Config) []string {
	var w []string
	snippet := cfg.Defaults.AuthSnippet != ""
	service := cfg.Defaults.AuthService != ""

	if snippet && !service {
		w = append(w, "auth_snippet is set but auth_service is not — post-login redirects will loop back to the auth portal. Name the auth backend with: splitdns set auth-service <name>")
	}
	if service && !snippet {
		w = append(w, "auth_service is set but auth_snippet is not — the (auth) snippet is an empty no-op, so auth does nothing. Set it with: splitdns set auth-snippet <path>")
	}
	if service {
		if _, ok := cfg.Services[cfg.Defaults.AuthService]; !ok {
			w = append(w, fmt.Sprintf("auth_service %q is not a defined service.", cfg.Defaults.AuthService))
		}
	}
	if snippet && service {
		used := false
		for _, s := range cfg.Services {
			if s.Auth {
				used = true
				break
			}
		}
		if !used {
			w = append(w, "the auth snippet is configured but no service uses it — opt one in with: splitdns update service <name> --auth")
		}
	}
	return w
}

func cmdUpdate(repoRoot, cfgPath string, args []string) int {
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <service> name.")
		hint("Usage: splitdns update service <name> [--fqdn ...] [--host ...] [--backend ...]")
		return 2
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	fs.StringVar(fqdn, "f", "", "alias for --fqdn")
	host := fs.String("host", "", "host that runs the service")
	fs.StringVar(host, "H", "", "alias for --host")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
	fs.StringVar(backend, "b", "", "alias for --backend")
	auth := fs.Bool("auth", false, "put this service behind the (auth) snippet")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// An update with no field flags is a no-op; tell the user instead of
	// silently reporting success.
	changed := 0
	fs.Visit(func(*flag.Flag) { changed++ })
	if changed == 0 {
		errf("Nothing to change for %q.", name)
		hint("Pass at least one of --fqdn, --host, or --backend.")
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
		case "auth":
			svc.Auth = *auth
		}
	})
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf(tick+" Updated service %q\n", name)
	return runSync(repoRoot, cfg, syncpkg.Incremental)
}

func cmdRemove(repoRoot, cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <service> name.")
		hint("Usage: splitdns remove service <name>")
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
		hint("Usage: splitdns %s service <name>", verb)
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
// Mode differs by mutation shape: service add/update/enable/disable use
// Incremental (write/update, never delete — they can't orphan anything a plain
// write wouldn't overwrite). remove-service and every host/domain/dns-host
// mutation use Complete, because they can leave orphaned files (a removed
// service's records, or a host/domain's now-dead cross-product of TLS
// snippets) that must be GC'd so the repo is left clean and `splitdns apply` won't
// refuse on drift.
func runSync(repoRoot string, cfg *config.Config, mode syncpkg.Mode) int {
	// Pre-flight: refuse before writing when a repo-wide precondition isn't met.
	if reason := syncBlockedReason(cfg); reason != "" {
		fmt.Fprintf(os.Stderr, cross+" Not synced: %s\n", reason)
		hint("  The change is saved in services.yaml. Run 'splitdns doctor --fix' once that's resolved.")
		return 1
	}

	// Read the auth snippet source (if configured). On failure, keep the
	// last-good generated snippet: refuse to regenerate the auth file rather than
	// silently reset it to the empty stub, which would drop auth on every
	// protected service fleet-wide. The rest of the sync proceeds.
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
		fmt.Println("Note: no dns_host set — run 'splitdns set dns-host <name>' (records can't be routed without it).")
	}
	if len(cfg.Domains) == 0 {
		fmt.Println("Note: no domains defined — run 'splitdns add domain <name>' (a service's fqdn must match a domain).")
	}
	for _, msg := range authConfigWarnings(cfg) {
		fmt.Printf("%s %s\n", warn, msg)
	}

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
	// 'splitdns doctor --fix'. add/update/remove proceed regardless (report-but-proceed).
	reportDrift(detectDrift(repoRoot, cfg, mf))
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

	// Collect the set of hosts that need `splitdns apply` run on them: the DNS host
	// (if its records changed) plus every caddy host whose files changed.
	needApply := map[string]bool{}
	if dnsDirty {
		needApply[cfg.Defaults.DNSHost] = true
	}
	for name := range caddyDirty {
		needApply[name] = true
	}

	fmt.Println("\nTo make changes live, run 'splitdns apply' on each host:")
	for _, name := range sortedKeysOf(needApply) {
		if name == self {
			fmt.Println("  splitdns apply  # here")
		} else {
			fmt.Printf("  on %s:  splitdns apply\n", name)
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
	// Migrate the pre-rename manifest so its tracked-file history (the GC
	// authority) survives the sd -> splitdns rename. Rebuild would lose
	// knowledge of files whose service was since removed.
	if legacy := filepath.Join(repoRoot, legacyManifestName); fileExists(legacy) && !fileExists(mfPath) {
		if err := os.Rename(legacy, mfPath); err == nil {
			fmt.Fprintf(os.Stderr, "Migrated %s -> %s (commit the rename).\n", legacyManifestName, manifestName)
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
		hint("  splitdns add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
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
const UsageText = `splitdns — Split-Horizon DNS (Manager)

Generates split-horizon DNS records and Caddy site blocks from a declarative
services.yaml. Operates on ~/docker by default; -C <dir> overrides.

Commands are verb-first: <verb> <noun> <args>.

Services (an app reached at an fqdn, on a host, under a domain):
  splitdns add     service <name> --fqdn <f> --host <h> --backend <b> [--auth]
  splitdns update  service <name> [--fqdn ...] [--host ...] [--backend ...] [--auth[=false]]
  splitdns remove  service <name>
  splitdns disable service <name>   Stop generating DNS/Caddy config for a service (keeps it in services.yaml).
  splitdns enable  service <name>   Re-enable a disabled service (regenerates its files).

Building blocks (a service references a host and a domain):
  splitdns add    host   <name> <ip>
  splitdns remove host   <name>
  splitdns add    domain <name>
  splitdns remove domain <name>
  splitdns set    dns-host <name>       Set the default resolver host for DNS records.
  splitdns set    auth-snippet <path>   Set the (auth) snippet source ('-' clears). Services opt in with --auth.

Other:
  splitdns apply                    Make config live on THIS host: restart pihole / validate+reload caddy. Run on each host. Refuses if the repo has drift.
  splitdns list [--all]             Show current hosts, domains, and services. Services default to THIS host; --all shows every host.
  splitdns verify [--all] [<fqdn>]  Check live DNS/Caddy per service. Defaults to services this host can check; --all includes the rest. Run on each host; needs docker.
  splitdns measure [--compare] [-n <runs>] [-w <warmup>] <service|fqdn|url>  Time the request breakdown (dns/connect/tls/ttfb) for a service or any URL. --compare A/Bs split-horizon vs public read-only (dns-host only, services only).
  splitdns doctor [--fix]           Audit the repo (gitignored files, Caddyfile imports, generated-file drift); --fix reconciles files and .gitignore.
  splitdns version
  splitdns completion <bash|zsh>    Print a shell completion script to stdout (see 'splitdns help completion' to install).
  splitdns help [<command>]         Show this text, or a command's help (same as <command> --help).

Global flags:
  -C, --chdir <dir>   Operate on <dir> instead of the default ~/docker.

Notes:
  - A host's name is its repo directory (e.g. host "pi" -> ./pi/), which must already exist.
  - Each domain gets a TLS snippet generated on every host, deriving cert paths from
    the convention caddy/data/certs/<domain>/{fullchain.cer,privkey.key}.
  - Config edits (add/update/remove/enable/disable) regenerate files automatically, then
    print which hosts to run 'splitdns apply' on to make the change live.
  - Removing a host or domain is refused while any service still references it.
`
