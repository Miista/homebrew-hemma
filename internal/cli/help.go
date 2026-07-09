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
	{"add service", `splitdns add service — declare a service and generate its DNS/Caddy config

Usage: splitdns add service <name> --fqdn <fqdn> --host <host> --backend <name:port> [--auth-mode forward|oidc]

Flags:
  -f, --fqdn <fqdn>       Public name the service is reached at (must match a declared domain).
  -H, --host <host>       Host (repo directory) that runs the service.
  -b, --backend <n:port>  reverse_proxy upstream, e.g. mealie:9000.
      --auth-mode <mode>  How the service authenticates: forward, oidc, or none (default none).
                          forward imports the (auth) snippet (Caddy forward-auth);
                          requires 'splitdns set auth-snippet <path>'. oidc renders a
                          PLAIN reverse_proxy (the app speaks OIDC itself — splitdns adds
                          no gate) and verifies read-only that an Authelia OIDC client exists.
      --auth              Back-compat shorthand for --auth-mode forward.

Regenerates files immediately, then prints which hosts need 'splitdns apply'.`},

	{"update service", `splitdns update service — change a service's fqdn, host, backend, or auth

Usage: splitdns update service <name> [--fqdn <fqdn>] [--host <host>] [--backend <name:port>] [--auth-mode forward|oidc|none]

Flags:
  -f, --fqdn <fqdn>       New public name (must match a declared domain).
  -H, --host <host>       New host (repo directory).
  -b, --backend <n:port>  New reverse_proxy upstream.
      --auth-mode <mode>  Set the auth mode: forward (import (auth) snippet), oidc
                          (plain reverse_proxy; app does OIDC itself), or none (clear).
      --auth[=false]      Back-compat shorthand: --auth = forward, --auth=false = none.

Only the given flags change; regenerated files and apply-hints follow.`},

	{"remove service", `splitdns remove service — remove a service and its generated files

Usage: splitdns remove service <name>`},

	{"enable service", `splitdns enable service — re-enable a disabled service (regenerates its files)

Usage: splitdns enable service <name>`},

	{"disable service", `splitdns disable service — stop generating config for a service (kept in services.yaml)

Usage: splitdns disable service <name>`},

	{"add host", `splitdns add host — declare a host (its name is its repo directory)

Usage: splitdns add host <name> <ip>

The directory ./<name>/ must already exist in the repo.`},

	{"remove host", `splitdns remove host — remove a host (refused while services reference it)

Usage: splitdns remove host <name>`},

	{"add domain", `splitdns add domain — declare a domain (generates a TLS snippet per host)

Usage: splitdns add domain <name>

Cert paths derive from caddy/data/certs/<domain>/{fullchain.cer,privkey.key}.`},

	{"remove domain", `splitdns remove domain — remove a domain (refused while services reference it)

Usage: splitdns remove domain <name>`},

	{"set dns-host", `splitdns set dns-host — set the default resolver host for DNS records

Usage: splitdns set dns-host <name>`},

	{"set auth-snippet", `splitdns set auth-snippet — set the (auth) snippet source

Usage: splitdns set auth-snippet <path>   (use '-' to clear)

<path> is a repo-relative Caddy file whose contents become the body of the
(auth) snippet generated on every host. Services opt in with 'add/update
service ... --auth', which emits 'import auth' in their site block. Clearing it
('-') regenerates an empty (auth) {} stub — services stay valid but unprotected.
The snippet is a normal generated file, so 'splitdns doctor' reports drift if
the source changes without a re-sync.`},

	{"set auth-service", `splitdns set auth-service — name the forward-auth backend service

Usage: splitdns set auth-service <name>   (use '-' to clear)

<name> is an existing service — the forward-auth portal (e.g. Authelia). Its
Caddy site block gains 'header_up X-Forwarded-Host {header.X-Forwarded-Host}',
so the original request host survives the hairpin through Caddy; without it,
post-login redirects loop back to the portal. Parallels 'set dns-host': one
repo-wide role named by service. Clearing it ('-') drops the header-preserve.`},

	{"list", `splitdns list — show hosts, domains, and services

Usage: splitdns list [--all]

By default the services list is filtered to those running on THIS host
(matched by local IP).

Flags:
  -a, --all   Show services on every host, not just this one.`},

	{"verify", `splitdns verify — check live DNS resolution per service

Usage: splitdns verify [--all] [<fqdn>]

By default it checks only services this host can verify (it is the resolver
or the service host); the rest are hidden. Pass a single <fqdn> to check just
that service. Run on each host to cover the whole chain; needs docker.

Flags:
  -a, --all   Also list services with nothing to check on this host.`},

	{"apply", `splitdns apply — make config live on THIS host

Usage: splitdns apply

Restarts pihole / validates+reloads caddy for this host's generated files.
Run it on each host after config changes. Refuses if the repo has drift.`},

	{"doctor", `splitdns doctor — audit the repo (gitignore, Caddyfile imports, drift)

Usage: splitdns doctor [--fix]

Flags:
  -f, --fix   Apply fixes: reconcile generated files and .gitignore entries.`},

	{"measure", `splitdns measure — time the HTTPS request breakdown (dns/connect/tls/ttfb)

Usage: splitdns measure [--compare] [-n <runs>] [-w <warmup>] <service|fqdn|url>

Flags:
  -c, --compare        A/B the split-horizon path vs the public path, both pinned
                       by IP (read-only; dns-host only, configured services only).
  -n, --runs <n>       Timed requests per leg (default 5).
  -w, --warmup <n>     Untimed warm-up requests first (default 3; 0 skips the
                       warm-up, so run 1 pays cold-start costs).

The target may be a configured service, a bare hostname, or any http(s) URL —
the latter two need no services.yaml. Requires bash, curl, awk.`},

	{"version", `splitdns version — print the version

Usage: splitdns version   (aliases: --version, -v)`},

	{"completion", `splitdns completion — print a shell completion script

Usage: splitdns completion <bash|zsh>

Writes a static completion script to stdout. It completes the top-level verbs,
the nouns (service/host/domain), and the known flags — not existing service,
host, or domain names (that would require invoking the tool).

Install (bash):
  splitdns completion bash | sudo tee /usr/share/bash-completion/completions/splitdns
Install (zsh, into a dir on your $fpath):
  splitdns completion zsh > "${fpath[1]}/_splitdns"

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
