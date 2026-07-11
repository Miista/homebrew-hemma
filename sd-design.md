# splitdns — Split-Horizon DNS (Manager) — Design Document

A Go CLI (`splitdns`) that generates and reconciles split-horizon DNS records (Pi-hole/dnsmasq)
and reverse-proxy site blocks (Caddy) for a homelab, from a single declarative source of truth
committed to a git repository.

> This document describes the current implementation. It was reconciled against the code
> (`internal/{config,plan,render,sync,manifest,cli}`) — section references in code comments
> (`design §N`) point back here. Historical note: the binary and CLI were formerly named `sd`;
> both are now `splitdns`, and the on-disk manifest/config names changed accordingly (§5).

## 1. Context and constraints

- The homelab is a **single git repository**, one directory per machine
  (e.g. `pi/`, `optiplex/`).
- Deployment is **`git pull`** per machine, then a live-reload step. This tool does **not**
  deploy or SSH anywhere — it only writes files into the local repo checkout. Making the
  written config *live* is a separate, explicit step run on each host: `splitdns apply` (§6.3),
  which restarts Pi-hole / validates + reloads Caddy. (This is a change from the original
  design, which left reload to an external deploy wrapper; `apply` folds that in as a command
  but still runs per-host and never SSHes.)
- A service's artifacts fan out across **two machine directories**: its DNS record lives on
  the resolver host (`defaults.dns_host`), its Caddy site block lives on the host that runs the
  service. Therefore the source of truth must be **repo-level**, not per-machine.
- DNS uses dnsmasq `local=` + `address=` directives via Pi-hole v6
  (`FTLCONF_misc_etc_dnsmasq_d: 'true'` must be set so `/etc/dnsmasq.d/*.conf` is sourced).
  `conf-dir` does **not** recurse into subdirectories — generated `.conf` files live directly
  in `dnsmasq.d/`, with `.generated.` in the filename as the marker (§4.1).
- Caddy serves wildcard certs pushed by an external acme.sh pipeline (every cert to every host).
  The tool never touches certs or ACME; it generates the per-domain `tls_<domain>` snippet (§4.3)
  and emits site blocks that `import` it.

## 2. Core model

- **The YAML file is the desired state and the sole source of truth.** It is committed to git.
  (The one exception: the auth snippet body is read from an external file referenced by
  `defaults.auth_snippet`, §4.5.)
- The tool is **reconcile-and-report**, Terraform/`make` style: every reconcile re-derives all
  output from the YAML as far as it validly can.
- There is **no checksum / lock / adopt gate.** Because every run re-derives all output from
  the YAML, drift cannot accumulate, and git operations (merges, edits from another clone,
  cherry-picks) "just work" — the tool has no opinion about *how* the YAML reached its current
  valid state.
- **Partial success is the norm.** Invalid entries are skipped and reported; valid entries are
  still generated. The only globally fatal condition is YAML that cannot be parsed at all.
- **Reconcile is not a user command.** Unlike the original design (which exposed
  `sync --incremental|--complete`), the reconcile engine is invoked automatically as the tail
  of every mutation. The mode is chosen by the mutation's shape, not by a user flag (§6.1). The
  user-facing surface for "make it match / make it live" is `doctor --fix` (repo) and `apply`
  (running daemons).

## 3. Source of truth: `services.yaml` (repo root)

The tool **owns** this file and rewrites it wholesale on mutation. Ordering is not preserved
and is unimportant. The file is machine-owned; human comments are not preserved (document
intent in a README, not in the YAML).

The in-memory `Config` uses a `map[string]Domain` for domains, but the on-disk (wire) form
serializes `domains` as a **plain sorted list of strings** — there is no per-domain data to
carry, so a list is cleaner than a map of empty objects.

```yaml
hosts:
  pi:       { ip: 192.0.2.1 }   # a host's name is its repo directory; `dir:` overrides only if they differ
  optiplex: { ip: 192.0.2.2 }

domains:
  - example.com   # TLS snippet name + cert path are derived from the domain
  - example.net

defaults:
  dns_host: pi                        # the single resolver host (set via: splitdns set dns-host)
  auth_snippet: authelia/forward-auth.caddy  # optional; auth directive copied into (auth) — §4.5
  auth_service: authelia              # optional; the service that IS the auth backend — §4.5

services:
  docs:
    fqdn: docs.example.com
    host: optiplex                    # machine that runs the service; Caddy site block goes in its dir
    backend: paperless:8000
    # disabled: true                  # optional; see §6.1 (enable/disable)
    # auth: forward                   # optional auth mode: forward|oidc (or legacy true=forward) — §4.5
```

Notes:
- The A record's IP is **looked up** from `hosts[<host>].ip`. The IP is declared once per
  machine and never repeated in service entries.
- A host's repo directory defaults to its name (`hosts` key). A `dir:` field exists for the
  rare case where directory ≠ name, but the convention (and the whole fleet today) is dir == name.
- `dns_host` is a **single repo-wide resolver** (`defaults.dns_host`, set via
  `splitdns set dns-host`). Every service's DNS record is routed through it; there is **no
  per-service override**. **Do not hardcode which host that is** — always read `defaults.dns_host`.
- A service's domain is chosen by matching its `fqdn`'s longest matching registrable domain
  suffix against the `domains` list; the TLS snippet name (`tls_<domain with dots→underscores>`)
  and cert path are derived from that domain — no per-domain config.

## 4. Generated output

The tool owns dedicated subdirectories. Filenames are human-readable and carry a `generated`
marker. Every generated file starts with the header
`# GENERATED by splitdns — do not edit. Source: services.yaml`.

### 4.1 DNS record (per service)
Path: `<dns_host>/pihole/data/dnsmasq.d/<service>.generated.conf`
*(directly in `dnsmasq.d/` — pihole's `conf-dir` does not recurse, so no `generated/` subdir;
the marker is in the filename. `<dns_host>` is the resolver host's repo directory.)*

Content (three directives):
```
# GENERATED by splitdns — do not edit. Source: services.yaml
local=/docs.example.com/
address=/docs.example.com/192.0.2.2
address=/docs.example.com/::
```

- `local=/<fqdn>/` makes dnsmasq **authoritative for the whole name**, so query types with no
  local record — notably the **HTTPS/SVCB (type 65)** record — return NODATA instead of being
  forwarded upstream and cached. This is required, not cosmetic: `address=` alone covers only
  A/AAAA; without `local=`, dnsmasq forwards the type-65 query to Cloudflare, and SVCB-aware
  clients (Safari, Chrome/Edge) follow its public endpoint hint, bypassing split-horizon.
  (This directive is **new since the original design**, which emitted only the two `address=`
  lines and thus leaked the SVCB record.)
- The A record points at `hosts[host].ip` (the machine that RUNS the service), **not** the
  dns_host.
- `address=/<fqdn>/::` is **always emitted** to suppress the public AAAA. `::` (unspecified) is
  correct; `::1` (loopback) is a bug — never emit `::1`.

### 4.2 Caddy site block (per service)
Path: `<host>/caddy/data/sites/<service>.caddy`
*(`<host>` is the service host's repo directory.)*

Content (plain service):
```
# GENERATED by splitdns — do not edit. Source: services.yaml
docs.example.com {
	import tls_example_com
	reverse_proxy paperless:8000
}
```

- `import tls_<domain>` uses the snippet name derived from the matched domain
  (`tls_` + domain with dots→underscores, e.g. `tls_example_com`).
- `backend` is emitted verbatim as the `reverse_proxy` upstream. It is expected to be a Docker
  network reference (`container:port`) resolvable on the serving machine's Docker network — not
  a LAN IP. The tool validates only shape (`^[A-Za-z0-9._-]+:[0-9]+$`), not reachability.
- A service's **auth mode** (`auth:`, §4.5) decides the gate. It is one of `forward`, `oidc`,
  or none (unset). Legacy `auth: true` still parses as `forward` and is re-emitted as the string
  form. When the mode is `forward`, the site body is wrapped in a catch-all `handle` block that
  imports the `(auth)` snippet before proxying, so Caddy runs the forward-auth check first and
  only proxies on success:
  ```
  docs.example.com {
  	import tls_example_com
  	handle {
  		import auth
  		reverse_proxy paperless:8000
  	}
  }
  ```
  When `public_paths` is set (§4.5), per-path `handle` blocks are emitted before the catch-all;
  Caddy `handle` blocks are mutually exclusive and first-match wins, so those paths are served
  directly without going through the auth gate:
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
  When the mode is `oidc` (or none), a **plain** `reverse_proxy` is emitted with **no** `import
  auth`: an OIDC app performs the login flow itself, so splitdns must add no second gate in front
  of it. `oidc` renders identically to a no-auth service in Caddy on purpose — the mode is still
  recorded in `services.yaml` so the protection intent stays legible (it is not silently "none").
- When the service **is** the auth backend (`name == defaults.auth_service`, §4.5), its
  `reverse_proxy` additionally preserves the inbound `X-Forwarded-Host`:
  ```
  auth.example.com {
  	import tls_example_com
  	reverse_proxy authelia:9091 {
  		header_up X-Forwarded-Host {header.X-Forwarded-Host}
  	}
  }
  ```
  Without this, the auth backend would reset `X-Forwarded-Host` to its own (auth) domain, so
  Authelia treats itself as the post-login target and loops the redirect. The value is whatever
  the trusted outer Caddy hop computed, so preserving it is not a spoofing vector.

### 4.3 TLS snippet (per host × domain)
Path: `<host>/caddy/data/tls/tls_<domain>.caddy`

Content:
```
# GENERATED by splitdns — do not edit. Source: services.yaml
(tls_example_com) {
	tls /etc/caddy/certs/example.com/fullchain.cer /etc/caddy/certs/example.com/privkey.key
}
```

The tool generates one snippet **per domain on every host** (the acme pipeline pushes every
cert to every host, so the snippet is valid everywhere). Cert paths follow the fixed convention
`/etc/caddy/certs/<domain>/{fullchain.cer,privkey.key}`. These files are owned/tracked under a
synthetic manifest key per domain (`@domain:<name>`, §5) and GC'd when a domain or host is
removed.

### 4.4 Caddyfile integration
Path: `<host>/caddy/data/splitdns.generated.caddy` (per host)

Each host's main `Caddyfile` must import `splitdns.generated.caddy`, whose content is fixed and
imports three things **in order**:
```
# GENERATED by splitdns — do not edit. Source: services.yaml
import splitdns.auth.generated.caddy
import tls/*.caddy
import sites/*.caddy
```
Order matters: the `(auth)` snippet (§4.5) and the `tls_*` snippets must be **defined before**
`sites/*.caddy` (the blocks that import them). `splitdns.auth.generated.caddy` sits directly in
`caddy/data/` (a sibling of the import file), so it is *not* swept up by the `sites/`/`tls/`
globs and must be imported by name — which is why it is listed explicitly and first.

The tool never edits the main `Caddyfile`; it writes this import file and tracks it under the
synthetic manifest key `@caddy-import`. Its content never changes after first write, but every
reconcile ensures it exists on every host. It is not counted as a service in output.

`splitdns doctor` checks that each host's `Caddyfile` contains the import line;
`splitdns doctor --fix` appends it if missing (and migrates the pre-rename `import
sd.generated.caddy`) (§6.4).

### 4.5 The `(auth)` snippet + auth backend

The `(auth)` snippet is a **generic** mechanism: its body can hold **any** Caddy auth directive
(`forward_auth`, `basic_auth`, a JWT check, an IP allowlist, …) and splitdns is agnostic to its
contents — it copies the file verbatim. The common case, and the one the "auth backend" role and
loop guard below are built around, is a `forward_auth` provider (Authelia et al.); those parts are
forward-auth-specific and noted as such.

Optional auth is a **repo-global** concern with **per-service opt-in**, split into two
generated pieces plus one config role:

**The `(auth)` snippet** — Path: `<host>/caddy/data/splitdns.auth.generated.caddy` (per host,
**always generated**, synthetic manifest key `@auth-snippet`).

- `defaults.auth_snippet` is a repo-relative path to a Caddy file holding an auth
  directive. Its contents are read (`config.LoadAuthSnippet`) and copied **verbatim** (each line
  indented one tab) into the body of a snippet named `(auth)`, generated byte-identically on
  **every host**:
  ```
  # GENERATED by splitdns — do not edit. Source: services.yaml
  (auth) {
  	forward_auth https://auth.example.com {
  		uri /api/authz/forward-auth
  		copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
  	}
  }
  ```
  The `forward_auth` target is a **public FQDN** resolved by the split-horizon DNS this tool
  already generates, so the same snippet is correct on every host — no per-host substitution.
  The body is **opaque** to splitdns; it is copied, not parsed.
- The snippet name is the concept — `"auth"`, never a provider name — so swapping the backing
  provider (Authelia → something else) is a one-file content change, not a rename everywhere.
- The file is **always generated**. With no `auth_snippet` set, an empty `(auth) {}` no-op stub
  is written. Combined with per-service `auth: true` emitting `import auth`, turning auth on/off
  is a **single-file content change** — site blocks never move.
- **Keep-last-good on unreadable source**: if `auth_snippet` is set but the file can't be read
  at reconcile time, `LoadAuthSnippet` returns the error **without** clearing the in-memory body,
  and `plan.PinAuthSnippetToDisk` rewrites the planned `(auth)` content to whatever is already on
  disk. The command reports the error and exits non-zero (report-but-proceed, §8) but the
  existing generated snippet stays in place — it is **not** reset to the empty stub. A path typo
  must never silently disable auth fleet-wide. This is the one place a generated file's content
  derives from an external source rather than purely from `services.yaml`.

**The auth backend** — `defaults.auth_service` names the single service that *is* the
forward-auth portal (parallels `dns_host`: one repo-wide role, named by service, set via
`splitdns set auth-service <name>`, `-` clears). That service's site block gets the
`X-Forwarded-Host` preservation described in §4.2.

**Loop guard** (§7): a service with a non-none auth mode **and** `name == defaults.auth_service`
is refused and skipped — protecting the portal with itself would recurse every auth subrequest.
(The guard keys on the service name, not on parsing the opaque snippet body.)

**Auth modes.** A service's `auth:` is a mode, not a bool (§4.2): `forward` wraps the site in a
`handle` block that imports the `(auth)` snippet; `oidc` renders a plain `reverse_proxy` (the app
does OIDC itself, splitdns adds no gate); none/unset is unprotected. Legacy `auth: true` is read
as `forward` and re-emitted as the string form. The mode is what makes an OIDC service's
protection legible despite rendering plain Caddy.

**`public_paths`.** A `forward`-auth service may declare a list of URL paths that bypass the auth
gate entirely. Each path is emitted as a `handle <path>` block before the catch-all `handle`
block, proxying directly to the backend. Caddy `handle` blocks are mutually exclusive and
first-match wins, so listed paths are never forwarded to the auth provider. Only meaningful when
`auth: forward`; ignored for all other modes.

**OIDC client validation (read-only).** For each `auth: oidc` service, splitdns reads the Authelia
config at `<auth_service host dir>/authelia/data/config/configuration.yml` (fixed path convention,
`config.DefaultAutheliaConfig`; host derived from `defaults.auth_service`) and checks that some
`identity_providers.oidc.clients[].redirect_uris` entry starts with `https://<fqdn>/`.
Missing → warn with the URI to register; config absent/unparseable → softer advisory; both
report-but-proceed. splitdns **never writes** the Authelia config — registering the OIDC client and
configuring the app's OIDC env are out of scope (the same internal-horizon boundary as §12). The
match is fqdn-only because callback paths are app-defined (`/login`, `/oidc/callback/`,
`/accounts/oidc/<provider>/...`) and unknown to splitdns.

**Auth groups.** A service's `auth:` also accepts an object form declaring which auth-provider
group names may access it:

```yaml
auth:
  mode: forward        # required in object form: forward|oidc|none
  groups: [admins]     # optional Authelia group names
```

The bool/string forms remain valid, and the tool re-emits the SHORT form (`auth: forward`) when
no groups are set — the object form appears in the YAML only when groups carry data. Set via
`--auth-groups a,b` on `add`/`update service` (`''` clears). Groups flow into the generated
access-control artifact (§4.6); groups with `mode: none` are a per-entry validation error (§7) —
there is no gate for them to apply to.

**Half-configured warnings** (`authConfigWarnings`, non-fatal, printed after a reconcile):
`auth_snippet` set but no `auth_service` (redirect-loop risk); `auth_service` set but no
`auth_snippet` (the `(auth)` block is a no-op stub); `auth_service` names a non-existent service;
fully configured but no service opted in; plus the OIDC advisories above. These are advisories —
auth still functions around them — so they warn, they don't block.

### 4.6 Generated access control (auth-provider artifact)

Path: `<auth_service host dir>/authelia/data/config/splitdns.access_control.generated.yml`
(next to `configuration.yml`; synthetic manifest key `@auth-access`).

splitdns generates the auth provider's access-control rules from the same `services.yaml` intent
it already renders Caddy gates from, so "who may reach this service" is declared once. Emitted
only when `defaults.auth_service` is set **and** there is something to say: `access_control`
rules for `auth: forward` services, `identity_providers.oidc.authorization_policies` for
`auth: oidc` services **with groups** (oidc services without groups need no provider-side
policy). When neither applies, the file is not planned at all and a previously generated one is
GC'd like any orphan. Content, in stable alphabetical-by-service order:

```yaml
# GENERATED by splitdns — do not edit. Source: services.yaml
access_control:
  default_policy: 'deny'
  rules:
    # per forward service: one bypass rule per public_paths entry first
    # (Authelia rules are first-match, mirroring the Caddy handle ordering)…
    - domain: 'status.example.com'
      resources:
        - '^/health([/?].*)?$'     # public_paths entry translated to a regex:
      policy: 'bypass'             # literal escaped, trailing /* → "and below", query allowed
    # …then the access rule:
    - domain: 'pihole.example.com'
      policy: 'one_factor'
      subject:
        - 'group:admins'           # omitted entirely when the service has no groups

identity_providers:
  oidc:
    authorization_policies:        # one section; one named policy per oidc-with-groups service
      app:
        default_policy: 'deny'
        rules:
          - policy: 'one_factor'
            subject:
              - 'group:users'
```

**Subject OR semantics** (easy to get backwards): in Authelia, a subject that is a string or a
flat list of strings is an **AND** of criteria; **OR** requires a list of *lists*. Membership in
ANY of a service's groups must grant access, so multiple groups are emitted as nested
single-element lists (`- ['group:a']` / `- ['group:b']`); a single group is a plain list item.

Boundary: splitdns generates this file but does **not** wire it in — telling Authelia to include
it, and pointing an OIDC client at its named `authorization_policy`, are provider-config steps
the OIDC validation (§4.5) warns about read-only. The provider config itself is never written
(same non-goal as §12).

**The auth-provider interface.** Everything provider-specific lives behind
`auth.Provider` (`internal/auth`): the provider's config-file convention path
(`ConfigPath`), the access-control artifact (`AccessControl(services) → (path,
content, ok)`, paths relative to the auth host's directory), and the read-only
validation of its own config (`ValidateConfig`). `plan` and `cli` are
provider-agnostic: plan places the returned `(path, content)` under the
`@auth-access` owner, cli prints the returned warnings. Providers are resolved
through a compile-time registry keyed by name, defaulting to `"authelia"` —
the interface boundary exists so a different provider (e.g. tinyauth) is a
drop-in. *Adding a provider*: implement the four `Provider` methods in a new
file in `internal/auth`, call `Register(yourProvider{})` from `init`, and keep
the single-writer rule — providers return content, they never write files.

## 5. Manifest

- **One file**, at the repo root: `splitdns-manifest.yaml`, **committed to git**.
  (Formerly `sd-manifest.yaml`; the pre-rename file is auto-migrated via `os.Rename` on first
  load, with a message to commit the rename.)
- Keyed by owner → list of repo-relative file paths that owner generated. Owners are service
  names **plus synthetic keys**: `@domain:<name>` (per-host TLS snippets), `@caddy-import` (the
  per-host import files), `@auth-snippet` (the per-host `(auth)` files), and `@auth-access`
  (the auth provider's access-control artifact, §4.6):

```yaml
docs:
  - optiplex/caddy/data/sites/docs.caddy
  - pi/pihole/data/dnsmasq.d/docs.generated.conf
"@domain:example.com":
  - optiplex/caddy/data/tls/tls_example_com.caddy
  - pi/caddy/data/tls/tls_example_com.caddy
"@caddy-import":
  - optiplex/caddy/data/splitdns.generated.caddy
  - pi/caddy/data/splitdns.generated.caddy
"@auth-snippet":
  - optiplex/caddy/data/splitdns.auth.generated.caddy
  - pi/caddy/data/splitdns.auth.generated.caddy
```

- Purpose:
  1. Distinguish generated files from hand-written ones (in manifest = ours).
  2. **Authority for safe deletion**: GC (Complete-mode reconcile) deletes only
     manifest-tracked files whose backing owner is gone or whose path is no longer desired.
     Files not in the manifest are **never** deleted.
  3. Detect missing/modified outputs for drift reporting (§9).
- **No per-file checksums.** Owned files are always overwritten on reconcile; that is the
  contract. Drift detection compares desired content to on-disk content at report time instead.
- **Rebuildable:** if the manifest is unparseable, it is rebuilt by re-deriving each owner's
  expected filenames from `services.yaml` and recording those that exist on disk (a warning is
  printed). A missing manifest is treated as empty, not fatal. Committing the manifest is what
  keeps since-deleted owners' files GC-able across the fleet.

## 6. Commands

All commands operate on `services.yaml` in `~/docker` by default; `-C`/`--chdir <dir>` runs as
if started in `<dir>` (git-style). There is no environment variable for the repo path. Commands
are **verb-first**: `<verb> <noun> <args>` (e.g. `add domain example.com`), routed through
`dispatchNoun` in `cli.go`. Flags have long and short aliases (e.g. `--fqdn`/`-f`, `--all`/`-a`).
`<command> --help` and `help <command>` print per-command help; the man page (`tools/genman`) is
compiled from the same strings so it can't drift.

### 6.1 Service commands

```
splitdns add     service <name> --fqdn <f> --host <h> --backend <b> [--auth]   (-f/-H/-b)
splitdns update  service <name> [--fqdn ...] [--host ...] [--backend ...] [--auth[=false]]
splitdns remove  service <name>
splitdns enable  service <name>
splitdns disable service <name>
```

- **`add`**: validates required flags, that the fqdn matches a defined domain, and that the host
  exists — all **before** persisting, so a mistyped command never writes a half-formed entry.
  Fails loud if the name or fqdn already exists. `--auth` opts the service into the `(auth)`
  snippet (§4.5). Then persists and reconciles (Incremental).
- **`update`**: fails if the service does not exist, or if no field flags were given (no-op is
  reported, not silently "succeeded"). Only explicitly-set fields are changed; `--auth[=false]`
  toggles forward-auth. Reconciles (Incremental).
- **`remove`**: deletes the entry from YAML, deletes that service's manifest-tracked files, and
  drops it from the manifest. A removal still needs a subsequent `apply` on the affected hosts to
  drop the vhost/record from the running daemons.
- **`disable`**: sets `disabled: true`, keeps the entry in YAML, and **deletes its generated
  files immediately** so it stops being served. **`enable`** clears the flag and regenerates.
  Disabled services are reported separately from validation-skipped ones.

**Reconcile mode by mutation shape** (`runSync`): add/update/enable use **Incremental**
(write/update, never delete). remove/disable use the targeted delete primitive. Every
host/domain/dns-host/auth-snippet/auth-service mutation (§6.2) uses **Complete**, because they
can orphan files (a removed service's records, or a host/domain's now-dead cross-product of TLS
snippets, or auth-snippet content that changed on every host) that must be GC'd/rewritten so the
repo is left clean and `apply` won't refuse on drift.

`update`, `remove`, `enable`, `disable` (and the read/host/domain commands) read existing state:
if `services.yaml` is **absent** (as opposed to present-but-empty), they refuse with a message
pointing at `add` and exit non-zero. `add` is exempt — it creates `services.yaml` when missing.
(A present-but-unparseable file is the §7 globally-fatal case; missing ≠ malformed.)

### 6.2 Building-block commands (host, domain) and `set`

```
splitdns add    host   <name> <ip>
splitdns remove host   <name>
splitdns add    domain <name>
splitdns remove domain <name>
splitdns set    dns-host     <name>
splitdns set    auth-snippet <path>          ('-' clears)
splitdns set    auth-service <name>          ('-' clears)
```

- **`add host <name> <ip>`**: both positional. The IP must be a valid address and unique across
  hosts. A host's name **is** its repo directory; that directory must already exist (a name with
  no matching directory is treated as a typo and rejected). Fails loud if the host exists.
- **`add domain <name>`**: name only; the TLS snippet name and cert path are derived (§4.3).
  Fails loud if the domain exists.
- **`set dns-host <name>`**: sets `defaults.dns_host`. The named host must already exist. Without
  it, a CLI-only bootstrap leaves `dns_host` unset and reconcile refuses to route records.
- **`set auth-snippet <path>`**: sets `defaults.auth_snippet` (the forward-auth block source,
  §4.5). `-` clears it (reverting `(auth)` to the empty stub).
- **`set auth-service <name>`**: sets `defaults.auth_service` (the portal service, §4.5). The
  named service must exist. `-` clears it (the auth backend's block stops preserving
  `X-Forwarded-Host`).
- **`remove host` / `remove domain`**: **refuse** while any service still references the target,
  and report the blockers. A host is referenced if any service names it as `host` **or** it is
  the resolver (`defaults.dns_host`); a domain is referenced if any service's fqdn matches it
  exactly or as a suffix. Reassign/remove those services first.

All of these mutate the YAML and then reconcile in **Complete** mode, leaving the repo clean.
They emit no per-host/per-domain output of their own beyond the derived TLS/auth snippets.

**Single-writer invariant:** the command handlers must **not** contain their own
generated-file-writing logic. They mutate the YAML struct in memory, persist it, then call the
shared reconcile engine (`internal/sync`), which is the only code that writes or deletes
generated files.

### 6.3 Making config live: `apply`

```
splitdns apply
```

`apply` makes the on-disk generated config **live on the host it runs on**. It is **host-split**:
it identifies which managed host it is by matching a local interface IP against `hosts[].ip`
(`localHost`), then performs the half (or halves) it owns:

- **DNS half** (if this host is `dns_host`): `docker restart pihole`. Pi-hole v6 does **not**
  reload `conf-dir` on `reloaddns`; a restart is required.
- **Caddy half** (if this host runs any non-disabled service): `caddy validate` **then**
  `caddy reload`. Validate runs first because it provisions the TLS app (loading cert files from
  disk), so a missing/wrong cert aborts here with a clear error instead of failing mid-reload.

`apply` **hard-refuses if the repo has drift** (the one command that does — everything else
reports-but-proceeds). The fix path is `doctor --fix` then `apply` again. Command output is
captured and shown only on failure; success prints just the ticks. Reload is idempotent, so
`apply` acts unconditionally on whatever this host owns. Run it on each affected host — it cannot
SSH.

### 6.4 Repo hygiene: `doctor`

```
splitdns doctor [--fix]     (-f)
```

`doctor` audits the repo (no docker needed) and, with `--fix`, repairs it. Three checks:

1. **Gitignore**: are any generated output paths swallowed by a `.gitignore` rule (e.g. a broad
   `**/data/**`)? Such files generate fine but never commit/deploy. `--fix` writes a managed
   negation block to the repo-root `.gitignore` (inside `# >>> splitdns managed >>>` markers) and
   re-verifies. (The auth-snippet source is loaded here too, so a bad `auth_snippet` path surfaces.)
2. **Caddyfile imports**: does each host's `Caddyfile` import `splitdns.generated.caddy`
   (§4.4)? `--fix` appends the import line if missing.
3. **Generated-file drift** (§9): missing / modified / orphaned generated files vs. what the
   plan says should exist. `--fix` runs a full **Complete** reconcile — rewriting missing/modified
   files and GC'ing orphaned tracked files (this is what the retired `sync --complete` did).

Exits non-zero if any problem remains.

### 6.5 Read-only inspection: `list`, `verify`, `measure`

```
splitdns list   [--all]                 (-a)
splitdns verify [--all] [<fqdn>]        (-a)
splitdns measure [--compare] [-n <runs>] [-w <warmup>] <service|fqdn|url>   (-c/--ab, -n, -w)
```

- **`list`**: plain inventory of hosts (marking `dns_host`), domains, and services. It is **not**
  a validity view — it runs no per-service planner checks and exits 0 (except on load failure).
  It warns first if `dns_host` is unset, marks disabled services `[disabled]`, and reports repo
  drift at the end. **Services default to the current host** (matched by local IP); `--all` shows
  every host. If the local IP matches no host, it falls back to showing everything.
- **`verify`** (§8): live resolution/serving checks, **host-split** like `apply`. Defaults to
  services this host can actually check (it is the resolver or the service host); `--all`
  includes the rest. An explicit `<fqdn>` always reports. Needs docker.
- **`measure`**: times the HTTPS request breakdown (dns / connect / tls / ttfb) for a service,
  fqdn, or arbitrary URL, over `-n` timed runs after `-w` untimed warm-ups (embedded
  `measure.sh`; needs bash/curl/awk). `--compare` A/Bs the split-horizon path against the public
  path via `curl --resolve` (read-only IP pinning, no toggling of pihole); it is restricted to a
  configured **service** and must run **on the dns-host** (the public-IP lookup uses DoH egress,
  sanctioned only on the resolver). The target may appear on either side of the flags.

### 6.6 Other

```
splitdns version | --version | -v
splitdns help [<command>]
splitdns -C, --chdir <dir> ...
```

## 7. Validation (per-entry, non-fatal)

The planner (`plan.Build`) validates each service independently, collecting errors rather than
stopping:

- `fqdn` is well-formed (regex) and its longest matching suffix is a defined domain (else skip).
- `host` references a defined host that has a non-empty `ip` (else skip).
- `defaults.dns_host` is set and references a defined host (else skip).
- `backend` matches `name:port` shape (else skip).
- `fqdn` is unique across services (collision → **both** conflicting entries skipped, reported).
- generated output paths are unique among survivors (path collision → skipped, reported).
- **auth loop guard**: a service with a non-none auth mode whose name equals `defaults.auth_service` is
  refused and skipped — protecting the auth backend with itself creates a redirect loop (§4.5).
- auth **groups with mode none** are refused (skip) — groups grant access through the generated
  access-control rules (§4.6), and with no auth gate there is nothing for them to apply to.

Disabled services are skipped with reason `"disabled"` and reported separately (not as errors).

Globally fatal (stop, generate nothing): `services.yaml` cannot be parsed. Manifest unparseable
is **not** fatal → rebuild (§5) and continue. An unreadable `auth_snippet` source is **not**
fatal either → keep-last-good + report (§4.5, §8).

## 8. Error reporting, exit codes, and `verify`

- **Collect all errors and report them together** at the end of a run — no fix-rerun-discover
  treadmill.
- Print a summary (`Synced N/M services.`), list changed and skipped services with reasons,
  print any half-configured-auth warnings (§4.5), and print per-host `apply` next-steps when
  files changed.
- **Exit non-zero if any entry was skipped or an error occurred** (including an unreadable
  `auth_snippet` source), so a deploy script can detect partial success. Output uses colored
  glyphs (`✓ ✗ ⚠`) only when stdout is a TTY and `NO_COLOR` is unset.

**`verify`** confirms behavior against the *running* system, not config syntax. It is host-split
(§6.5) and, per applicable host, runs (all via `docker exec`):

- **Resolver side** (in the `pihole` container):
  - `dig +short A <fqdn> @127.0.0.1` must equal the service host IP (else the conf isn't loaded
    and pihole is forwarding upstream).
  - Inspect `pihole.log` to confirm the answer was `config` (answered locally), not `forwarded`
    or `cached`.
  - `dig +short AAAA <fqdn> @127.0.0.1` must be `::` (never `::1`).
  - `dig +short -t TYPE65 <fqdn> @127.0.0.1` must be **NODATA** (else the HTTPS/SVCB record leaks
    a public endpoint — what the §4.1 `local=` line prevents).
- **Service-host side** (in the `caddy` container):
  - `caddy adapt` the Caddyfile and confirm the fqdn appears in the *adapted* config (proves the
    imports actually loaded the generated site block).
  - `caddy validate` passes.
  - the generated site's `reverse_proxy` uses the declared backend.
  - a live local HTTPS request (`curl --resolve <fqdn>:443:127.0.0.1`) returns a real status
    code — the running-state proof that the reloaded config actually serves the host.

Exits non-zero if any check fails. (The original design used a host-side `getent` client-view
check; the implementation instead does the richer in-container checks above, which don't depend
on the host having `dnsutils`.)

## 9. Recoverability and drift

- Any interrupted command leaves a state fully repaired by a subsequent Complete reconcile
  (`doctor --fix`). No transactional file I/O is required beyond atomic single-file writes;
  reconciliation is the recovery mechanism.
- All file writes are **atomic**: write to a temp file in the same dir, `fsync`, `rename`
  (`config.atomicWrite`), so an interrupted write never leaves a half-written `.conf`/`.caddy`.
- **Deletion order**: DNS records (dnsmasq `.conf`) are deleted **before** Caddy blocks, so an
  interrupted deletion leaves the safe residual "the name no longer resolves" rather than "it
  resolves to a host that no longer routes it."
- **Drift** (`internal/cli/drift.go`) is a pure repo concept (no docker):
  - *Missing*: a desired generated file is absent on disk.
  - *Modified*: a generated file's on-disk content differs from what the plan would write. (This
    is how an edited `auth_snippet` source surfaces: the generated `(auth)` file is flagged
    modified until re-synced.)
  - *Orphaned*: a manifest-tracked file no longer desired that still exists (the GC target).
  Most commands report drift and proceed; only `apply` refuses on it (§6.3).

## 10. On-disk paths

Output subpaths under each host's directory are **fixed** (not per-host configurable, though the
directory name itself can be overridden via `hosts[].dir`):

- DNS records: `<host-dir>/pihole/data/dnsmasq.d/<service>.generated.conf`
- Caddy sites: `<host-dir>/caddy/data/sites/<service>.caddy`
- TLS snippets: `<host-dir>/caddy/data/tls/tls_<domain>.caddy`
- Caddy import: `<host-dir>/caddy/data/splitdns.generated.caddy`
- Auth snippet: `<host-dir>/caddy/data/splitdns.auth.generated.caddy`

where `<host-dir>` is the host's `ResolvedDir` (its `dir:` or, by convention, its name).

## 11. Package layout

- `config`   — load, mutate, persist `services.yaml`; schema structs; the atomic-write
  primitive; `LoadAuthSnippet` (reads the external auth-snippet source).
- `plan`     — validate entries → desired set of `(path, content)` files; collect per-entry
  errors; synthetic `@domain:` / `@caddy-import` / `@auth-snippet` / `@auth-access` owners;
  `PinAuthSnippetToDisk` (keep-last-good).
- `auth`     — the pluggable auth-provider boundary (`Provider` interface + registry, §4.6);
  the Authelia implementation (access-control artifact rendering, read-only OIDC validation).
- `render`   — pure string rendering for the dnsmasq `.conf`, Caddy site, TLS snippet, import,
  and `(auth)` snippet files.
- `manifest` — load/save/rebuild the manifest; the deletion authority.
- `sync`     — the reconcile `Engine`: diff desired vs manifest, write/delete, GC (Complete mode),
  atomic writes. **The only writer/deleter of generated files.**
- `cli`      — command parsing (stdlib `flag`), wiring, all user-facing output; help text
  (`UsageText` + `HelpTopics`, single-sourced with the man page); thin `main.go` calls `cli.Run`.
- `tools/genman` — compiles the CLI help strings into `man/splitdns.1.gz` at release time.

## 12. Explicit non-goals

- No SSH/orchestration. The tool writes into the local repo checkout only; `apply` acts on the
  local host's daemons and must be run per-host.
- No reachability/health checks of backends (only `name:port` shape validation).
- No per-file checksums or "you hand-edited my output" gate (drift *reports* modified files; it
  does not block except at `apply`).
- No editing of machines' main Caddyfiles beyond appending the one import line under
  `doctor --fix`.
- No parsing/validation of the auth snippet body — it is copied verbatim and is opaque
  to splitdns (§4.5).
- **No public-horizon management.** splitdns sets up the *internal* horizon only (Pi-hole record
  + Caddy block). It deliberately does **not** create, verify, or warn about the *public*
  horizon — the public DNS + tunnel ingress that must already exist for a split-horizon setup to
  be meaningful. On this homelab that public side is a `cloudflare.io/hostname` label on the
  service's container, read off the docker socket by cloudflared-wrapper. Rationale for keeping
  it out of splitdns:
  - splitdns only writes files it *owns* (its Pi-hole/Caddy subdirs). The tunnel label lives in
    a hand-maintained `docker-compose.yml` — a foreign, human-owned file. Surgically editing it
    (preserving comments/formatting) is a categorically riskier operation than rewriting an owned
    file wholesale.
  - Any awareness of the public side (even a read-only warning) couples splitdns to one specific
    tunnel tool's private label convention, eroding its generator-agnostic core (Pi-hole + Caddy,
    nothing else). Swap tunnels and splitdns would be wrong.
  - The tunnel tool already reads the docker socket and is better placed to warn when a container
    is served but has no ingress.

  Gotcha this documents: a service can have a correct *internal* horizon (splitdns did its job)
  yet be publicly broken because the `cloudflare.io/hostname` label is missing — the FQDN then
  falls back to the zone apex publicly (e.g. `auth.palmund.net` → `palmund.net`). The label is a
  manual, per-service step that lives with the compose file, not with splitdns.
