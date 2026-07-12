# hemma — homelab DNS, Caddy, and auth config manager

A small Go CLI that generates **split-horizon DNS records** (Pi-hole / dnsmasq),
**reverse-proxy site blocks** (Caddy), and **auth-provider config** (Authelia access control)
for a homelab, from a single declarative `services.yaml` committed to the homelab git repository.

> **Renamed:** this tool was previously `splitdns` (and before that `sd`). It outgrew its name —
> it now manages DNS, Caddy, and auth-provider config. The `splitdns` command keeps working as an
> alias, and `hemma doctor --fix` migrates a splitdns-era repo (manifest name, generated
> filenames, Caddyfile import lines, .gitignore markers) automatically.

`hemma` is **reconcile-and-report**, Terraform/`make` style: every reconcile re-derives all
output from the YAML so the generated files match the declared state. The mutation commands only
write files into your repo checkout; making them live is a separate step — `hemma apply`, run
per host, restarts/reloads that host's local daemons (pihole, caddy, the auth provider). Nothing
SSHes anywhere. Deployment stays `git pull` then `hemma apply` per machine.

See [`design.md`](design.md) for the full design rationale.

## Why

A homelab service's artifacts fan out across **three** machine directories:

- its **DNS record** lives on the resolver host (the Pi),
- its **Caddy site block** lives on the host that actually runs the service,
- its **access policy** — who may reach it — lives on the auth host, in the auth provider's
  config (Authelia `access_control` rules for forward-auth services, named OIDC
  `authorization_policies` for apps that do OIDC themselves).

Hand-maintaining all three, in sync, across a monorepo is error-prone — and the auth side
fails worst: under a default-deny policy a forgotten rule is an outage, and under a permissive
one a forgotten group restriction quietly grants every authenticated user access (the
"why can my wife reach pihole?" class of bug). Routing and access also drift apart: the Caddy
gate and the Authelia rule for the same service live in different files on different hosts,
with nothing keeping them consistent.

`hemma` makes one YAML file the source of truth for all of it: where a service runs, how it is
reached, **and who may reach it** — DNS, proxy, and access policy are generated from the same
`services.yaml` entry, and `doctor` cross-checks the parts hemma deliberately does not own
(OIDC client registrations, users and their groups).

## Install

### Homebrew

```sh
brew tap Miista/hemma
brew install hemma
```

(The tap repo is [`Miista/homebrew-hemma`](https://github.com/Miista/homebrew-hemma); Homebrew strips
the `homebrew-` prefix. The formula is renamed via the tap's `formula_renames.json`, so an
existing `splitdns` install follows the rename on `brew update && brew upgrade`. The formula
installs a `splitdns` alias symlink.)

### Debian / Ubuntu (apt)

The tools are published to a signed [Cloudsmith](https://cloudsmith.io) apt
repository (`guldmund/stable`). One-time setup:

```sh
sudo install -d /usr/share/keyrings
curl -1sLf https://dl.cloudsmith.io/public/guldmund/stable/gpg.key \
  | sudo gpg --dearmor -o /usr/share/keyrings/guldmund-stable-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/guldmund-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/guldmund/stable/deb/debian any-version main" \
  | sudo tee /etc/apt/sources.list.d/guldmund-stable.list
sudo apt update && sudo apt install hemma
```

Upgrading from the pre-rename package: `sudo apt install hemma` — the `hemma` package
`Provides`/`Replaces` `splitdns` and installs a `/usr/bin/splitdns` alias symlink.

The repo is distro-agnostic (`debian any-version`), so the same line works on
any Debian/Raspberry Pi OS/Ubuntu release. After setup, updates arrive via
regular `apt upgrade`. Older `.deb`s are on the
[releases page](https://github.com/Miista/homebrew-hemma/releases).

### From source

Requires Go 1.26+.

```sh
go build -o hemma .   # or: go install .
```

The tool operates on `services.yaml` in **`~/docker`** by default; `-C <dir>`
overrides it (git-style). There is no environment variable to configure. Check
the version with `hemma version`.

### Shell completions

The Homebrew formula and the Debian package install bash + zsh completions
automatically. For a from-source build, generate them yourself:

```sh
# bash
hemma completion bash | sudo tee /usr/share/bash-completion/completions/hemma > /dev/null
# zsh (into a directory on your $fpath)
hemma completion zsh > "${fpath[1]}/_hemma"
```

Completion covers the verbs, the `service`/`host`/`domain` nouns, and the known
flags (existing service/host/domain names are not completed — that would require
invoking the tool).

## Quick start

Bootstrap a usable `services.yaml` entirely from the CLI — no hand-editing required:

```sh
# Commands operate on ~/docker by default; pass -C <dir> for another checkout.

# 1. Declare the hosts (a host's name IS its repo directory, which must exist)
hemma add host resolver 192.0.2.1
hemma add host appbox   192.0.2.2

# 2. Choose the default resolver host (whose dnsmasq receives the records)
hemma set dns-host resolver

# 3. Declare the domains (hemma generates each one's TLS snippet on every host)
hemma add domain example.com
hemma add domain example.net

# 4. Add a service (mutates YAML, then regenerates the files)
hemma add service docs --fqdn docs.example.com --host appbox --backend paperless:8000

# 5. (optional) Put services behind the (auth) snippet
#    Point at a Caddy file containing any auth directive (forward_auth, basic_auth, …), then opt services in.
hemma set auth-snippet auth-snippet.caddy
hemma update service docs --auth-mode forward
#    Or, for an app that does OIDC itself (hemma adds no gate, only validates the client):
hemma update service app --auth-mode oidc

# 5b. (optional) Mint auth credentials — print-only, native Go crypto; paste the snippets
#     into the auth provider's config by hand (hemma never writes those files).
hemma create app oidc app /oidc/callback   # OIDC client id + secret + configuration.yml snippet
hemma create user alice                    # argon2id password hash + users_database.yml snippet

# 6. There is no separate sync step: every mutation regenerates files as its tail.
#    To reconcile the whole repo any time (regenerate missing/modified, GC orphans):
hemma doctor --fix
```

This generates, for `docs`:

```
# resolver/pihole/data/dnsmasq.d/docs.generated.conf
# GENERATED by hemma — do not edit. Source: services.yaml
local=/docs.example.com/
address=/docs.example.com/192.0.2.2
address=/docs.example.com/::
```

```
# appbox/caddy/data/sites/docs.caddy
# GENERATED by hemma — do not edit. Source: services.yaml
docs.example.com {
	import tls_example_com
	reverse_proxy paperless:8000
}
```

Note the A record points at the **service host** IP (`appbox`), while the file is written into
the **resolver host** (`resolver`) directory. The `address=/<fqdn>/::` line always suppresses the public
AAAA record so IPv6-preferring clients can't bypass split-horizon, and the `local=/<fqdn>/` line makes
dnsmasq authoritative for the whole name so HTTPS/SVCB (type 65) queries return NODATA instead of
leaking the public endpoint to SVCB-aware clients (Safari, Chrome).

## Auth (optional)

Each service records **how** it authenticates via an auth **mode**, set with
`--auth-mode <mode>` on `add`/`update service` (or the back-compat `--auth`, which means
`forward`):

| Mode | services.yaml | Caddy rendered | hemma's role |
|------|---------------|----------------|-----------------|
| `none` (default) | *(omitted)* | plain `reverse_proxy` | nothing |
| `forward` | `auth: forward` | `handle` block with `import auth` before `reverse_proxy` | adds the forward-auth gate (the `(auth)` snippet) |
| `oidc` | `auth: oidc` | **plain** `reverse_proxy` (no `import auth`) | adds **no** gate; the app speaks OIDC itself. hemma only **validates** (read-only) that an Authelia OIDC client exists |

`oidc` renders a plain `reverse_proxy` — identical to `none` in Caddy — because the app
performs the OIDC flow itself; hemma must not add a second (forward-auth) gate in front of
it. Recording the mode as `oidc` (rather than leaving it `none`) keeps the intent legible: the
`list` `AUTH` column and services.yaml show `oidc`, so a protected service never looks like an
unprotected one.

The `AUTH` column in `hemma list` shows the mode (`forward` / `oidc` / `-`).

**Back-compat:** a legacy `auth: true` in services.yaml still parses (as `forward`) and is
re-emitted as `auth: forward` on the next mutation; `auth: false`/absent = `none`.

**Auth groups.** `--auth-groups a,b` (on `add`/`update service`; `''` clears) restricts a
`forward` or `oidc` service to members of the given auth-provider (Authelia) groups. Groups are
stored in the object YAML form (`auth: {mode, groups}`; the short `auth: forward` form is kept
when no groups are set) and flow into a generated Authelia access-control file on the
`auth_service` host — `authelia/data/config/hemma.access_control.generated.yml` — containing
`access_control` rules for forward services (bypass rules for their `public_paths`, then a
`one_factor` rule; multiple groups are OR'd) and named
`identity_providers.oidc.authorization_policies` for oidc services with groups. hemma
generates the file but does not wire it into Authelia; include it in the Authelia config and
point each OIDC client's `authorization_policy` at its service name (the OIDC validation warns
if you forget). `hemma doctor` warns — with the exact `X_AUTHELIA_CONFIG` value to paste into
the auth host's compose file — until the container actually loads the file, and while a
hand-written `access_control:` section remains in `configuration.yml` alongside the generated
one. Groups with `auth: none` are a validation error.

**OIDC validation (read-only, hemma does NOT configure OIDC).** For each `auth: oidc`
service, hemma reads the Authelia config at
`<auth_service host dir>/authelia/data/config/configuration.yml` and checks that some
`identity_providers.oidc.clients[].redirect_uris` entry starts with
`https://<fqdn>/` (callback paths are app-defined, so the match is fqdn-only). If none does, it warns; if the config is missing/unparseable
it emits a softer advisory and proceeds (report-but-proceed). hemma never writes that file —
**registering the OIDC client and configuring the app's OIDC env are out of scope.** If
`auth_service` is unset, it notes that OIDC clients can't be verified.

### Forward auth

The `(auth)` snippet mechanism (used by `auth: forward`) is **generic** — its body can be any Caddy auth directive
(`basic_auth`, a JWT check, an IP allowlist, …); hemma copies it verbatim and is agnostic to
its contents. This section works through the common case: putting services behind a
[Caddy `forward_auth`](https://caddyserver.com/docs/caddyfile/directives/forward_auth)
provider (Authelia, Authentik, oauth2-proxy, …). The design keeps one shared,
substitution-free snippet for the whole fleet:

- **`defaults.auth_snippet`** (`hemma set auth-snippet <path>`) points at a Caddy file whose
  contents are your `forward_auth` block. Its body is copied verbatim into a generated
  **`caddy/data/hemma.auth.generated.caddy`** on *every* host, wrapped as a Caddy snippet named
  `(auth)` and imported before the site blocks. Because the target is a **public FQDN**
  (e.g. `https://auth.example.com`) resolved by your split-horizon DNS, the same snippet is
  byte-identical on every host — no per-host substitution.
- **`--auth-mode forward`** (or the shorthand `--auth`) on `add`/`update service` sets
  `auth: forward`, which emits `import auth` in that service's site block (before
  `reverse_proxy`, so the auth check runs first). Only opted-in services are protected.
- The `(auth)` file is **always generated** — an empty `(auth) {}` no-op stub when no snippet is
  set — so toggling auth never rewrites site blocks, and `doctor` tracks it as an ordinary
  generated file (it flags drift if you edit the source without reconciling — `hemma doctor --fix`).
- If the configured `auth_snippet` source is **missing/unreadable at reconcile time**, hemma keeps
  the last-good generated file (warns, exits non-zero) rather than silently resetting to the empty
  stub — a path typo can never disable auth fleet-wide.
- A service that **is** the auth backend (`defaults.auth_service`) is refused any auth mode
  (forward or oidc), preventing a redirect loop — the portal must be reachable un-gated.

Given `auth-snippet.caddy`:

```
forward_auth https://auth.example.com {
	uri /api/authz/forward-auth
	copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
}
```

`hemma set auth-snippet auth-snippet.caddy` generates on every host:

```
# <host>/caddy/data/hemma.auth.generated.caddy
# GENERATED by hemma — do not edit. Source: services.yaml
(auth) {
	forward_auth https://auth.example.com {
		uri /api/authz/forward-auth
		copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
	}
}
```

and a service added with `--auth` gets:

```
# appbox/caddy/data/sites/status.caddy
# GENERATED by hemma — do not edit. Source: services.yaml
status.example.com {
	import tls_example_com
	handle {
		import auth
		reverse_proxy gatus:8080
	}
}
```

### Public paths (auth exemptions)

Some services need certain paths to be publicly reachable even when behind `auth: forward` — for
example a `/health` endpoint that a monitoring tool polls without credentials. Add `public_paths`
to the service entry in `services.yaml`:

```yaml
services:
  status:
    fqdn: status.example.com
    host: appbox
    backend: gatus:8080
    auth: forward
    public_paths:
      - /health
```

Each listed path is emitted as a `handle <path>` block **before** the auth-gated catch-all.
Caddy `handle` blocks are mutually exclusive and first-match wins, so those paths go straight to
the backend without touching the auth provider:

```
status.example.com {
	import tls_example_com
	handle /health {
		reverse_proxy gatus:8080
	}
	handle {
		import auth
		reverse_proxy gatus:8080
	}
}
```

`public_paths` is ignored for `auth: none` and `auth: oidc` services (those have no auth gate to
exempt from). Set it directly in `services.yaml`; there is no CLI flag for it.

## Commands

```
hemma [-C <dir>] add    service <name> --fqdn <f> --host <h> --backend <b> [--auth-mode forward|oidc] [--auth-groups <g1,g2>]
hemma [-C <dir>] update service <name> [--fqdn ...] [--host ...] [--backend ...] [--auth-mode forward|oidc|none] [--auth-groups <g1,g2>]
hemma [-C <dir>] remove service <name>
hemma [-C <dir>] enable  service <name>
hemma [-C <dir>] disable service <name>

hemma [-C <dir>] add    host   <name> <ip>
hemma [-C <dir>] remove host   <name>
hemma [-C <dir>] add    domain <name>
hemma [-C <dir>] remove domain <name>
hemma [-C <dir>] set    dns-host <name>
hemma [-C <dir>] set    auth-snippet <path>
hemma [-C <dir>] set    auth-service <name>

hemma [-C <dir>] create app oidc <app_name> [callback_path]
hemma [-C <dir>] create user <username>

hemma [-C <dir>] list   [--all]
hemma [-C <dir>] verify [--all] [<fqdn>]
hemma [-C <dir>] apply
hemma [-C <dir>] doctor [--fix]
hemma [-C <dir>] measure [--compare] [-n <runs>] <service|fqdn|url>
hemma [-C <dir>] version
hemma            completion <bash|zsh>

  -C <dir>   operate on <dir> instead of the default ~/docker
```

| Command | Behavior |
| --- | --- |
| `add` | Fail if name/fqdn already exists. Mutate YAML, then regenerate. `--auth-mode forward` (or the `--auth` shorthand) opts the service into the `(auth)` snippet (imports it). |
| `update` | Fail if the service doesn't exist. Only the flags you pass are changed. Then regenerate. `--auth-mode` / `--auth[=false]` sets or clears the auth mode. |
| `remove` | Drop the service from YAML, delete its tracked files, drop it from the manifest. |
| `disable` / `enable` | `disable` keeps the entry in YAML but deletes its generated files immediately; `enable` clears the flag and regenerates. |
| `add host` / `add domain` | Declare a host / domain. `add host <name> <ip>` (the name is its repo directory, which must already exist; the IP must be unique). `add domain <name>` — hemma generates the domain's TLS snippet on every host immediately. |
| `remove host` / `remove domain` | **Refuses** while any service still references it (and lists the blockers). Idempotent otherwise. |
| `set dns-host <name>` | Set the default resolver host (the one whose dnsmasq receives records). |
| `set auth-snippet <path>` | Set the `(auth)` snippet source (a repo-relative Caddy file holding any auth directive). Pass `-` to clear it (regenerates an empty no-op stub). See [Forward auth](#forward-auth-optional). |
| `set auth-service <name>` | Name the service that IS the forward-auth portal (e.g. Authelia); its site block preserves `X-Forwarded-Host` through the hairpin. Pass `-` to clear. |
| `create app oidc` | Mint OIDC client credentials (id, secret, digest) + a ready-to-paste provider config snippet. Print-only. See [Auth](#auth-optional). |
| `create user` | Interactively hash a new user's password (argon2id) + print the users-database snippet. Print-only. |
| `list` | The overview of the home: hosts, domains, services (with an `AUTH` column showing `forward` / `oidc` / `-`), and the auth **groups** — each group's users and the services restricted to it, including orphans (a group with services but no users, or users but no services). The services list defaults to those on **this** host (matched by local IP); `--all` shows every host. Read-only. |
| `apply` | Make synced config live on THIS host: restart pihole (resolver), `caddy validate` + reload (service hosts), and validate + restart the auth provider (auth host). Refuses on repo drift. Run on each host. |
| `doctor [--fix]` | Audit the repo: gitignored generated files, Caddyfile imports, generated-file drift, auth config consistency (OIDC clients registered, policies referenced, groups exist on real users). `--fix` reconciles files, .gitignore, and legacy-name migration. |
| `measure` | Time the request breakdown (dns/connect/tls/ttfb) for a service or URL; `--compare` A/Bs split-horizon vs public. Read-only. |
| `completion <bash\|zsh>` | Print a static shell completion script to stdout (verbs, nouns, and flags). See [Shell completions](#shell-completions). |
| `verify` | Check the **live** system per service, host-split. On the resolver (in the pihole container): `dig` A == the service host's IP and answered locally (not forwarded/cached, per `pihole.log`), AAAA == `::`, and HTTPS/SVCB (type 65) == NODATA. On the service host (in the caddy container): the fqdn appears in the `caddy adapt`-ed config, `caddy validate` passes, the site's `reverse_proxy` uses the declared backend, and a local HTTPS request (`curl --resolve`) returns a real status. Defaults to services this host can check; `--all` includes the rest. Run **on each host** after a deploy. Needs docker. |

There is **no `sync` command**: the reconcile engine runs automatically as the tail of every
mutation (add/update/remove/enable/disable, and the host/domain/`set` commands), reporting a
summary, the changed service names, and per-host `hemma apply` next-steps. The full repo-wide
reconcile — regenerate missing/modified files and GC orphans — is `hemma doctor --fix`.

All commands are **verb-first**: `<verb> <noun> <args>` (e.g. `add domain example.com`).
`update`, `remove`, and the other commands that read existing state refuse with a guiding message
(and non-zero exit) when there is no `services.yaml` in the directory; `add` creates it.

## Source of truth: `services.yaml`

`hemma` **owns** this file and rewrites it wholesale on mutation. Ordering and human comments
are not preserved — document intent in this README, not in the YAML.

```yaml
hosts:
  resolver: { ip: 192.0.2.1 }   # the host's name is its repo directory
  appbox:   { ip: 192.0.2.2 }

domains:
  - example.com   # TLS snippet + cert path are derived from the name
  - example.net

defaults:
  dns_host: resolver          # the single resolver host (set via: hemma set dns-host)
  auth_snippet: auth-snippet.caddy   # optional; auth directive copied into (auth) on every host

services:
  docs:
    fqdn: docs.example.com
    host: appbox        # host that runs the service; Caddy site block goes in its dir
    backend: paperless:8000
    auth: forward       # optional; forward|oidc|none (or legacy true=forward)
    # auth:               # object form when access is restricted to groups:
    #   mode: forward     #   required: forward|oidc|none
    #   groups: [admins]  #   optional; Authelia groups allowed access (OR'd)
    public_paths:       # optional; paths exempt from auth (only meaningful with auth: forward)
      - /health
```

Output paths are fixed: `<dns-host>/pihole/data/dnsmasq.d/<service>.generated.conf` (directly in
`dnsmasq.d/` — pihole's `conf-dir` does not recurse, so the `generated` marker is in the
filename) and `<host>/caddy/data/sites/<service>.caddy`, where `<host>` is the host's name
(which **is** its repo directory).

## Manifest

`hemma-manifest.yaml` (repo root, **committed to git**) records which files each service
generated. It is the authority for safe deletion: only manifest-tracked files are ever deleted,
and files not in the manifest are never touched. If lost or corrupt, it is rebuilt from
`services.yaml` on the next run.

## Validation & partial success

Each service is validated independently; invalid entries are **skipped and reported** while valid
ones are still generated. The only globally fatal condition is a `services.yaml` that cannot be
parsed. A run that skipped anything prints a summary and **exits non-zero**, so a deploy script
can detect partial success:

```
Synced 11/12 services.
1 skipped:
  • docs: unknown host "appbx" — defined hosts: appbox, resolver
```

## Prerequisites (one-time, manual)

`hemma` writes only its own generated files (dnsmasq records, Caddy `sites/` and `tls/`
snippets, the `(auth)` snippet, the auth access-control artifact); it never rewrites a host's
main `Caddyfile`. Each host's `Caddyfile` must contain the single line:

```
import hemma.generated.caddy
```

`hemma doctor` checks for it, and `hemma doctor --fix` appends it when missing (the one edit
hemma makes to a Caddyfile). The generated `hemma.generated.caddy` then imports, in order, the
`(auth)` snippet (`hemma.auth.generated.caddy`), `tls/*.caddy`, and `sites/*.caddy` — so the
snippets are always defined before the site blocks that reference them.

`hemma` generates the `(tls_<domain>)` snippets into `tls/` (deriving cert paths from the
convention `caddy/data/certs/<domain>/`), so you no longer hand-write them. Caddy serves the
wildcard certs from an external acme.sh pipeline — `hemma` never touches certs or ACME.

### .gitignore and the `data/` directory

`hemma`'s generated files live under `data/` directories (`caddy/data/sites/`, etc.). If your repo
ignores runtime data with a broad rule like `**/data/**`, those generated files are **silently
ignored by git** — they generate fine but never commit or deploy. `hemma` detects this and warns
on every reconcile (and via `hemma doctor`), printing the negation rules to add:

```
!**/data/
!**/data/**/
!**/data/**/*.generated.conf
!**/caddy/data/**/*.caddy
!**/data/**/*.generated.yml
```

`hemma doctor` reports the problem and the exact lines; **`hemma doctor --fix`** writes them for you,
into a managed block in the repo-root `.gitignore` (between `# >>> hemma managed >>>` markers,
preserving your other rules), then re-verifies. `hemma` only touches `.gitignore` when you
explicitly run `doctor --fix`. Runtime data (databases, caches, certs) stays ignored.

## Making changes live: `hemma apply`

A changed bind-mounted file does **not** restart a container on its own. After `git pull` on a
host, run `hemma apply` there: it restarts pihole (if this host is the resolver), validates and
reloads Caddy (if this host serves anything), and validates + restarts the auth provider (if
this host runs the `auth_service`) — each with validate-before-reload so a bad config aborts
instead of taking the service down. `hemma verify` then checks live DNS per service:

```sh
hemma apply     # on each host after a pull
hemma verify    # assert A == host IP and AAAA == :: against the running resolver
```

## Development

```sh
go build ./...
go test ./...
go vet ./...
```

Package layout (`internal/`):

| Package | Responsibility |
| --- | --- |
| `config` | Load / mutate / persist `services.yaml`; schema structs; atomic writes. |
| `plan` | Validate entries → desired `(path, content)` set; collect per-entry errors. |
| `manifest` | Load / save / rebuild the manifest; safe-deletion authority. |
| `render` | Templates for the dnsmasq `.conf` and Caddy `.caddy` files. |
| `auth` | The pluggable auth-provider boundary (`Provider` interface + registry): access-control rendering, credential minting, config validation. Authelia is the sole implementation today. |
| `sync` | The reconcile engine — the **only** code that writes or deletes generated files. |
| `cli` | Command parsing and wiring; thin `main.go`. |

## Non-goals

No SSH/orchestration (`apply` acts only on the host it runs on), no backend health checks, no
per-file checksums, no editing of machines' main Caddyfiles, and no writing of the auth
provider's own config or users database (`create` prints snippets; you paste them).

## License

[MIT](LICENSE) © Søren Guldmund
