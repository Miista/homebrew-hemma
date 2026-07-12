package cli

import (
	"fmt"
	"os"
	"strings"
)

// HelpTopic is one command's help text. HelpTopics is the single source of
// truth: `-h/--help` prints from it, and tools/genman compiles it into the
// man page — edit here and both stay in sync.
type HelpTopic struct {
	Cmd  string // e.g. "measure", "add service"
	Text string // shown verbatim; first line is the one-line summary
}

var HelpTopics = []HelpTopic{
	{"add service", `hemma add service — declare a service and generate its DNS/Caddy config

Usage: hemma add service <name> --fqdn <fqdn> --host <host> --backend <name:port> [--auth-mode forward|oidc] [--auth-groups <g1,g2>]

Flags:
  -f, --fqdn <fqdn>       Public name the service is reached at (must match a declared domain).
  -H, --host <host>       Host (repo directory) that runs the service.
  -b, --backend <n:port>  reverse_proxy upstream, e.g. mealie:9000.
      --auth-mode <mode>  How the service authenticates: forward, oidc, or none (default none).
                          forward imports the (auth) snippet (Caddy forward-auth);
                          requires 'hemma set auth-snippet <path>'. oidc renders a
                          PLAIN reverse_proxy (the app speaks OIDC itself — hemma adds
                          no gate) and verifies read-only that an Authelia OIDC client exists.
      --auth-groups <gs>  Comma-separated auth provider group names allowed access; flows into
                          the generated access-control rules (multiple groups are OR'd).
                          Requires an auth mode (forward or oidc).
      --auth              Back-compat shorthand for --auth-mode forward.

Regenerates files immediately, then prints which hosts need 'hemma apply'.`},

	{"update service", `hemma update service — change a service's fqdn, host, backend, or auth

Usage: hemma update service <name> [--fqdn <fqdn>] [--host <host>] [--backend <name:port>] [--auth-mode forward|oidc|none] [--auth-groups <g1,g2>]

Flags:
  -f, --fqdn <fqdn>       New public name (must match a declared domain).
  -H, --host <host>       New host (repo directory).
  -b, --backend <n:port>  New reverse_proxy upstream.
      --auth-mode <mode>  Set the auth mode: forward (import (auth) snippet), oidc
                          (plain reverse_proxy; app does OIDC itself), or none (clear).
      --auth-groups <gs>  Set the comma-separated auth provider groups (OR'd in the
                          generated access-control rules). '' clears them. Requires an
                          auth mode (forward or oidc).
      --auth[=false]      Back-compat shorthand: --auth = forward, --auth=false = none.

Only the given flags change; regenerated files and apply-hints follow.`},

	{"remove service", `hemma remove service — remove a service and its generated files

Usage: hemma remove service <name>`},

	{"enable service", `hemma enable service — re-enable a disabled service (regenerates its files)

Usage: hemma enable service <name>`},

	{"disable service", `hemma disable service — stop generating config for a service (kept in services.yaml)

Usage: hemma disable service <name>`},

	{"add host", `hemma add host — declare a host (its name is its repo directory)

Usage: hemma add host <name> <ip>

The directory ./<name>/ must already exist in the repo.`},

	{"remove host", `hemma remove host — remove a host (refused while services reference it)

Usage: hemma remove host <name>`},

	{"add domain", `hemma add domain — declare a domain (generates a TLS snippet per host)

Usage: hemma add domain <name>

Cert paths derive from caddy/data/certs/<domain>/{fullchain.cer,privkey.key}.`},

	{"remove domain", `hemma remove domain — remove a domain (refused while services reference it)

Usage: hemma remove domain <name>`},

	{"set dns-host", `hemma set dns-host — set the default resolver host for DNS records

Usage: hemma set dns-host <name>`},

	{"set auth-snippet", `hemma set auth-snippet — set the (auth) snippet source

Usage: hemma set auth-snippet <path>   (use '-' to clear)

<path> is a repo-relative Caddy file whose contents become the body of the
(auth) snippet generated on every host. Services opt in with 'add/update
service ... --auth', which emits 'import auth' in their site block. Clearing it
('-') regenerates an empty (auth) {} stub — services stay valid but unprotected.
The snippet is a normal generated file, so 'hemma doctor' reports drift if
the source changes without a re-sync.`},

	{"set auth-service", `hemma set auth-service — name the forward-auth backend service

Usage: hemma set auth-service <name>   (use '-' to clear)

<name> is an existing service — the forward-auth portal (e.g. Authelia). Its
Caddy site block gains 'header_up X-Forwarded-Host {header.X-Forwarded-Host}',
so the original request host survives the hairpin through Caddy; without it,
post-login redirects loop back to the portal. Parallels 'set dns-host': one
repo-wide role named by service. Clearing it ('-') drops the header-preserve.`},

	{"create app", `hemma create app oidc — generate OIDC client credentials for an app

Usage: hemma create app oidc <app_name> [callback_path]

Mints a client id and client secret (72 chars, RFC 3986 unreserved charset)
plus the secret's PBKDF2-SHA512 digest, then prints a ready-to-paste
identity_providers.oidc.clients snippet. Print-only: the auth provider's
configuration is never written.

If <app_name> matches a configured service, its real fqdn is used for the
redirect URI, and — when the service has auth groups — the snippet references
the generated named authorization policy instead of one_factor. Otherwise the
redirect host is derived from the configured domains (<app_name>.<first domain
alphabetically>); with no domains configured the command refuses — add the
service or a domain first.
[callback_path] defaults to /CHANGEME (apps differ; fill in the real path).`},

	{"create user", `hemma create user — interactively create an auth user (hash + snippet)

Usage: hemma create user <username>

Prompts for an email and a password (hidden, entered twice), hashes the
password with argon2id (Authelia defaults), and prints a users-database entry
to paste in. Print-only: the users database is hand-owned and secret-bearing,
so hemma never writes it. Assign groups by editing the pasted entry's
'groups: []' — 'hemma doctor' cross-checks them against services.yaml.`},

	{"list", `hemma list — overview: hosts, domains, services, and auth groups

Usage: hemma list [--all]

By default the services list is filtered to those running on THIS host
(matched by local IP).

When any auth group exists, a Groups section follows the services table: the
union of the auth provider's users database (user -> groups, read-only) and
services.yaml (service -> auth groups), one block per group, showing its users
and the services restricted to it. One-sided groups still list (services but
no users = nobody can access; users but no services). If the users database is
missing or unreadable, a services-only view is shown with a note. Usernames
only; never password hashes or emails.

Flags:
  -a, --all   Show services on every host, not just this one.`},

	{"verify", `hemma verify — check live DNS resolution per service

Usage: hemma verify [--all] [<fqdn>]

By default it checks only services this host can verify (it is the resolver
or the service host); the rest are hidden. Pass a single <fqdn> to check just
that service. Run on each host to cover the whole chain; needs docker.

Flags:
  -a, --all   Also list services with nothing to check on this host.`},

	{"apply", `hemma apply — make config live on THIS host

Usage: hemma apply

Restarts pihole (resolver host), validates + reloads caddy (service hosts),
and validates + restarts the auth provider (the host running auth_service) —
each validate-before-reload, so a bad config aborts instead of taking the
service down. Run it on each host after config changes. Refuses if the repo
has drift.`},

	{"doctor", `hemma doctor — audit the repo (gitignore, Caddyfile imports, drift, auth)

Usage: hemma doctor [--fix]

Flags:
  -f, --fix   Apply fixes: reconcile generated files and .gitignore entries.

The auth checks are read-only advisories (hemma never writes the provider's
config, users database, or compose file); each one prints the exact fix to
paste in — e.g. the X_AUTHELIA_CONFIG value that wires the generated
access-control file into the auth container.`},

	{"measure", `hemma measure — time the HTTPS request breakdown (dns/connect/tls/ttfb)

Usage: hemma measure [--compare] [-n <runs>] [-w <warmup>] <service|fqdn|url>

Flags:
  -c, --compare        A/B the split-horizon path vs the public path, both pinned
                       by IP (read-only; dns-host only, configured services only).
  -n, --runs <n>       Timed requests per leg (default 5).
  -w, --warmup <n>     Untimed warm-up requests first (default 3; 0 skips the
                       warm-up, so run 1 pays cold-start costs).

The target may be a configured service, a bare hostname, or any http(s) URL —
the latter two need no services.yaml. Requires bash, curl, awk.`},

	{"version", `hemma version — print the version

Usage: hemma version   (aliases: --version, -v)`},

	{"completion", `hemma completion — print a shell completion script

Usage: hemma completion <bash|zsh>

Writes a static completion script to stdout. It completes the top-level verbs,
the nouns (service/host/domain), and the known flags — not existing service,
host, or domain names (that would require invoking the tool).

Install (bash):
  hemma completion bash | sudo tee /usr/share/bash-completion/completions/hemma
Install (zsh, into a dir on your $fpath):
  hemma completion zsh > "${fpath[1]}/_hemma"

The Debian package and Homebrew formula install these scripts for you.`},
}

// helpFor returns the help text for a topic like "measure" or "add service".
func helpFor(topic string) (string, bool) {
	for _, t := range HelpTopics {
		if t.Cmd == topic {
			return t.Text, true
		}
	}
	return "", false
}

// maybeHelp handles -h/--help anywhere in a command's args, and `help <cmd>`.
// Returns true (after printing) if help was requested.
func maybeHelp(cmd string, rest []string) bool {
	want := false
	if cmd == "help" && len(rest) > 0 {
		want = true
		cmd, rest = rest[0], rest[1:]
	}
	for _, a := range rest {
		if a == "-h" || a == "--help" {
			want = true
		}
	}
	if !want {
		return false
	}
	topic := cmd
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		if _, ok := helpFor(cmd + " " + rest[0]); ok {
			topic = cmd + " " + rest[0]
		}
	}
	if text, ok := helpFor(topic); ok {
		fmt.Fprintln(os.Stderr, text)
	} else {
		usage()
	}
	return true
}
