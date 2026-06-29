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

	"shd/internal/config"
	"shd/internal/manifest"
	"shd/internal/plan"
	syncpkg "shd/internal/sync"
)

const (
	configName   = "services.yaml"
	manifestName = "shd-manifest.yaml"
)

// Version is the build version, overridden at release time via
// -ldflags "-X shd/internal/cli.Version=...".
var Version = "dev"

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
	// Operate on the current directory by default; -C <dir> changes it
	// (git-style). Strip a leading -C/<dir> before dispatching.
	repoRoot := "."
	if args[0] == "-C" {
		if len(args) < 2 {
			errf("The -C flag requires a directory argument.")
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

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("shd", Version)
		return 0
	case "add":
		return dispatchNoun(repoRoot, cfgPath, "add", rest)
	case "update":
		return dispatchNoun(repoRoot, cfgPath, "update", rest)
	case "remove":
		return dispatchNoun(repoRoot, cfgPath, "remove", rest)
	case "set":
		return dispatchSet(cfgPath, rest)
	case "list":
		return cmdList(cfgPath, rest)
	case "verify":
		return cmdVerify(cfgPath, rest)
	case "sync":
		return cmdSync(repoRoot, cfgPath, rest)
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
		hint("Usage: shd %s service|host|domain ...", verb)
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
		hint("Usage: shd %s service|host|domain ...", verb)
		return 2
	}
	// Reached only if a verb/noun combo fell through (shouldn't happen).
	errf("Unsupported: %s %s.", verb, noun)
	return 2
}

// dispatchSet routes `set <thing> <args>`; currently only `set dns-host`.
func dispatchSet(cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing what to set — expected dns-host.")
		hint("Usage: shd set dns-host <name>")
		return 2
	}
	switch args[0] {
	case "dns-host":
		return cmdSetDNSHost(cfgPath, args[1:])
	default:
		errf("Unknown setting %q — expected dns-host.", args[0])
		return 2
	}
}

func cmdAdd(repoRoot, cfgPath string, args []string) int {
	// Usage puts <service> before flags; Go's flag pkg stops at the first
	// positional, so split it off first.
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <service> name.")
		hint("Usage: shd add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
		return 2
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	host := fs.String("host", "", "host that runs the service")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
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
		hint("Usage: shd add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
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
	cfg.Services[name] = config.Service{FQDN: *fqdn, Host: *host, Backend: *backend}
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("✓ Added service %q\n", name)
	return runSyncAfterMutation(repoRoot, cfg, name)
}

// runSyncAfterMutation runs an incremental sync following an add/update of
// service `name` and prints the second phase line. The caller has already
// printed the first ("✓ Added service X"). On success: "✓ Synced service X".
// If sync can't proceed (e.g. no dns_host): "✗ Not synced: <reason>", making
// clear the YAML change WAS saved — only generation was deferred.
func runSyncAfterMutation(repoRoot string, cfg *config.Config, name string) int {
	if reason := syncBlockedReason(cfg); reason != "" {
		fmt.Fprintf(os.Stderr, "✗ Not synced: %s\n", reason)
		hint("  The change is saved in services.yaml. Run 'shd sync' once that's resolved.")
		return 1
	}
	p := plan.Build(cfg)
	mf := loadManifest(repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	if _, err := eng.Reconcile(p, syncpkg.Incremental); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("✓ Synced service %q\n", name)
	return 0
}

// printVerbose lists every generated file grouped by its owner — services by
// name, then a "shared" group for the per-domain TLS snippets — each path
// marked by what the sync did to it (+ created, ~ updated, = unchanged), plus
// a trailing group for deletions.
func printVerbose(p *plan.Plan, res *syncpkg.Result) {
	mark := map[string]string{}
	for _, f := range res.Created {
		mark[f] = "+"
	}
	for _, f := range res.Updated {
		mark[f] = "~"
	}
	for _, f := range res.Unchanged {
		mark[f] = "="
	}

	group := func(title string, files []plan.File) {
		if len(files) == 0 {
			return
		}
		fmt.Printf("  %s\n", title)
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			m := mark[path]
			if m == "" {
				m = "="
			}
			fmt.Printf("    %s %s\n", m, path)
		}
	}

	// Services in sync order, then shared TLS owners.
	for _, name := range res.Synced {
		group(name, p.Files[name])
	}
	for _, owner := range sortedKeysOf(p.Files) {
		if plan.IsDomainOwner(owner) {
			group("shared TLS for "+plan.DomainOf(owner), p.Files[owner])
		}
	}

	if len(res.Deleted) > 0 {
		fmt.Println("  deleted")
		for _, d := range res.Deleted {
			fmt.Printf("    - %s\n", d)
		}
	}
}

// syncBlockedReason returns a human-readable reason a sync cannot run at all
// (a global precondition), or "" if sync may proceed. Per-entry skips are not
// blockers — only repo-wide preconditions are.
func syncBlockedReason(cfg *config.Config) string {
	if len(cfg.Services) > 0 && cfg.Defaults.DNSHost == "" {
		return "no dns_host is set, so DNS records can't be routed. Set the resolver with: shd set dns-host <name>"
	}
	return ""
}

func cmdUpdate(repoRoot, cfgPath string, args []string) int {
	name, args, ok := leadingName(args)
	if !ok {
		errf("Missing the <service> name.")
		hint("Usage: shd update service <name> [--fqdn ...] [--host ...] [--backend ...]")
		return 2
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fqdn := fs.String("fqdn", "", "service fqdn")
	host := fs.String("host", "", "host that runs the service")
	backend := fs.String("backend", "", "reverse_proxy upstream name:port")
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
		}
	})
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf("✓ Updated service %q\n", name)
	return runSyncAfterMutation(repoRoot, cfg, name)
}

func cmdRemove(repoRoot, cfgPath string, args []string) int {
	if len(args) < 1 {
		errf("Missing the <service> name.")
		hint("Usage: shd remove service <name>")
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
	fmt.Printf("✓ Removed service %q\n", name)
	if n := len(res.Deleted); n > 0 {
		fmt.Printf("✓ Deleted %d generated %s\n", n, plural(n, "file"))
	} else {
		fmt.Printf("✓ No generated files to delete\n")
	}
	return 0
}

func cmdSync(repoRoot, cfgPath string, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	incremental := fs.Bool("incremental", false, "write/update only, never delete (default)")
	complete := fs.Bool("complete", false, "incremental plus GC of orphaned tracked files")
	verbose := fs.Bool("verbose", false, "list every generated file, not just changes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *incremental && *complete {
		errf("The --incremental and --complete flags are mutually exclusive.")
		return 2
	}
	mode := syncpkg.Incremental
	if *complete {
		mode = syncpkg.Complete
	}

	cfg, code := loadExisting(cfgPath, "sync")
	if cfg == nil {
		return code
	}
	return runSync(repoRoot, cfg, mode, *verbose)
}

// runSync builds the plan, reconciles, prints a summary, and returns the exit
// code per design §8. By default it reports only what changed; verbose lists
// every generated file.
func runSync(repoRoot string, cfg *config.Config, mode syncpkg.Mode, verbose bool) int {
	// Pre-flight: refuse the whole sync (rather than silently skipping every
	// affected service) when a repo-wide precondition isn't met.
	if reason := syncBlockedReason(cfg); reason != "" {
		errf("Cannot sync: %s", reason)
		return 1
	}

	p := plan.Build(cfg)
	mf := loadManifest(repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}

	res, err := eng.Reconcile(p, mode)
	if err != nil {
		errf("%v", err)
		return 1
	}

	synced, total := len(res.Synced), res.Total
	fmt.Printf("Synced %d/%d services.\n", synced, total)

	if verbose {
		printVerbose(p, res)
	} else {
		for _, name := range res.Synced {
			fmt.Printf("  • %s\n", name)
		}
	}

	if len(res.Skipped) > 0 {
		fmt.Printf("%d skipped:\n", len(res.Skipped))
		for _, name := range sortedSkip(res.Skipped) {
			fmt.Printf("  • %s: %s\n", name, res.Skipped[name])
		}
	}

	if len(res.Skipped) > 0 {
		return 1
	}
	return 0
}

// loadManifest loads the manifest, rebuilding it if unparseable (design §5/§7).
func loadManifest(repoRoot string, cfg *config.Config) *manifest.Manifest {
	mfPath := filepath.Join(repoRoot, manifestName)
	mf, ok := manifest.Load(mfPath)
	if !ok {
		fmt.Fprintf(os.Stderr, "Warning: %s is unreadable — rebuilding it from %s.\n", manifestName, configName)
		mf = manifest.Rebuild(mfPath, repoRoot, plan.Build(cfg))
	}
	return mf
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
		hint("  shd add service <name> --fqdn <fqdn> --host <host> --backend <name:port>")
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
	fmt.Fprint(os.Stderr, `shd — Split-Horizon DNS (Manager)

Generates split-horizon DNS records and Caddy site blocks from a declarative
services.yaml. Operates on the file in the current directory by default.

Commands are verb-first: <verb> <noun> <args>.

Services (an app reached at an fqdn, on a host, under a domain):
  shd add    service <name> --fqdn <f> --host <h> --backend <b>
  shd update service <name> [--fqdn ...] [--host ...] [--backend ...]
  shd remove service <name>

Building blocks (a service references a host and a domain):
  shd add    host   <name> <ip>
  shd remove host   <name>
  shd add    domain <name>
  shd remove domain <name>
  shd set    dns-host <name>    Set the default resolver host for DNS records.

Other:
  shd sync   [--incremental | --complete]
  shd list                     Show current hosts, domains, and services (with validity).
  shd verify                   Check live DNS resolution per service (run on the resolver host; needs docker).
  shd version
  shd help

Global flags:
  -C <dir>   Run as if shd were started in <dir> (default: current directory).

Notes:
  - A host's name is its repo directory (e.g. host "pi" -> ./pi/), which must already exist.
  - On sync, each domain gets a TLS snippet generated on every host, deriving cert paths from
    the convention caddy/data/certs/<domain>/{fullchain.cer,privkey.key}.
  - Removing a host or domain is refused while any service still references it.
`)
}
