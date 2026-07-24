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

With no flags (and stdin a terminal) an interactive editor opens instead: the
fqdn is pre-filled with <name>.<domain> when exactly one domain is defined and
validated live (must match a domain, must not collide with another service),
the host and auth mode are pickers, and auth groups are a multi-select built
from the groups that actually exist — on users in the auth provider's users
database (members shown per group) and on other services — plus a 'new group…'
escape hatch. On submit it prints the collected fields and runs the exact same
validation and sync path as the flags form; Ctrl-C touches nothing.
Non-interactive callers must pass flags.

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

With no flags (and stdin a terminal) an interactive editor opens instead:
every field is pre-filled with the service's current values (Enter keeps
them), the host and auth mode are pickers, and auth groups are a multi-select
built from the groups that actually exist — on users in the auth provider's
users database (members shown per group) and on other services — plus a
'new group…' escape hatch. On submit it prints the changed fields (old → new)
and runs the exact same validation and sync path as the flags form; 'no
changes' or Ctrl-C touches nothing. Non-interactive callers must pass flags.

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

Usage: hemma add host <name> <ip> [--ssh <dest>]

The directory ./<name>/ must already exist in the repo.

Flags:
      --ssh <dest>   ssh(1) destination 'hemma deploy' uses to reach the host —
                     a verbatim alias/user@host/anything ssh accepts (put ports,
                     users, keys in ssh_config). Defaults to the host's name.`},

	{"update host", `hemma update host — change a host's ip or ssh destination

Usage: hemma update host <name> [--ip <ip>] [--ssh <dest>]

Only the given flags change. Changing the ip regenerates every DNS record
pointing at the host; changing the ssh destination touches nothing generated.

Flags:
      --ip <ip>      New IP address (must be valid and unique across hosts).
      --ssh <dest>   New verbatim ssh(1) destination for 'hemma deploy'.
                     Pass '-' (or '') to clear it back to the default (the
                     host's name).`},

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

The PUBLIC column says whether an FQDN is reachable from the internet ('yes')
or is internal-only ('no'). hemma generates the internal horizon (Pi-hole record
+ Caddy block); the public side is the tunnel's business, so this is read —
never written — from the host's docker-compose.yml: a 'cloudflare.io/hostname'
label whose value is the FQDN means yes, its absence means no. Set
defaults.public_label in services.yaml to match another tunnel tool's label
key, or to 'none' to drop the column. The column is also dropped when no host's
compose file can be read; '?' marks one host whose compose is missing or
unparseable, so a read failure is never reported as 'no'.

PUBLIC reports what IS. To declare what SHOULD be, set 'public: true|false' on
the service in services.yaml; 'hemma doctor' then reports any contradiction.

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

Two phases: first VALIDATE everything this host owns (caddy config, auth
provider config) — any failure aborts with nothing touched; then act:
restart pihole (resolver host), reload caddy (service hosts), restart the
auth provider (the host running auth_service). A bad config can never cost
a pihole restart or leave the host half-applied. Run it on each host after
config changes. Refuses if the repo has drift.`},

	{"deploy", `hemma deploy — push-based fan-out: pull + apply on every host over ssh

Usage: hemma deploy [<host> ...]

Makes the fleet match origin, in three strictly ordered phases:

  Phase 0 — probe:  'ssh <dest> true' to every REMOTE host. ANY host
                    unreachable over ssh aborts before anything is pulled —
                    a clear "unreachable" instead of git-pull noise later.
  Phase 1 — pull:   'git -C ~/docker pull --ff-only' on every target host.
                    ANY failure (divergence, dirty remote tree, unreachable
                    host, ssh auth) aborts the ENTIRE deploy before phase 2 —
                    no runtime state changed anywhere. After the pulls, every
                    host must be on the same commit (a racing push aborts too).
  Phase 2 — apply:  'hemma apply' per host — remotes first, THIS host last,
                    so a resolver restart can't break DNS-resolved ssh to the
                    remaining hosts. A failure on one host is reported and the
                    fan-out continues; exit is non-zero with a per-host summary.

Targets default to every host with a role (runs a service, or is the
dns_host); pass names to restrict. Hosts are reached with
'ssh -o BatchMode=yes <dest>' where <dest> is the host's ssh: field
(default: its name); a host whose IP matches this machine runs locally.
Refuses up front if the LOCAL repo is dirty or has unpushed commits —
deploying means "make the fleet match origin", so push first.`},

	{"doctor", `hemma doctor — audit the repo (gitignore, Caddyfile imports, drift, auth,
deploy readiness)

Usage: hemma doctor [--fix]

Flags:
  -f, --fix   Apply fixes: reconcile generated files and .gitignore entries.

The auth checks are read-only advisories (hemma never writes the provider's
config, users database, or compose file); --fix cannot resolve them. Each
advisory prints a headline (the consequence), the mechanism, and a fixed
'fix:'/'then:' grammar — the exact edit to paste in (e.g. the
X_AUTHELIA_CONFIG value that wires the generated access-control file into
the auth container) and the follow-up command (usually 'hemma apply').

The public-horizon checks read each host's docker-compose.yml (never write it)
and compare the tunnel-ingress labels against services.yaml, in descending
severity:

  * AUTH BYPASS — a forward-auth service whose ingress points DIRECT at the
    container. The tunnel never traverses Caddy, so the (auth) gate hemma
    generated never runs and the service is publicly reachable with no
    authentication. Counts as a problem.
  * DECLARED vs OBSERVED — the service says 'public: true|false' and the labels
    say otherwise. Services with no 'public' field are silent, so the check is
    opt-in. Counts as a problem.
  * ORPHAN INGRESS — a hostname served publicly in a managed domain with no
    service entry, so it has NO internal horizon: on the LAN it resolves via
    public DNS, leaves the network, and hairpins back through the tunnel.
    Informational; does not fail doctor.

Each carries the exact label to add or remove. A missing or unparseable compose
file yields no findings at all — absence of evidence is not evidence. Configure
the label keys with defaults.public_label and defaults.public_proxy_label
('none' disables either).

The deploy-readiness check verifies each remote deploy target has a host key
in THIS machine's known_hosts ('ssh-keygen -F' against the host's ip and its
ssh destination — either counts). 'hemma deploy' sshes with BatchMode=yes,
which cannot answer the first-connection trust prompt, so a missing entry
fails deploy outright. Not --fix-able (hemma does not own known_hosts): the
recipe is to connect once interactively and verify the fingerprint. doctor
never connects anywhere — whether ssh auth actually WORKS is verified by
deploy itself, in its phase-0 probe.`},

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
