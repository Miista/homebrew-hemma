# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`hemma` is a Go CLI that generates split-horizon DNS records (Pi-hole/dnsmasq `address=` directives)
and Caddy site blocks for a homelab, from a single declarative `services.yaml` committed to the homelab
git repo. It is **reconcile-and-report** (Terraform/`make` style): every sync re-derives all output from
the YAML. Sync and the mutation commands only write files into the local repo checkout; making them live
is the separate `hemma apply` command, run per host, which restarts/reloads that host's local daemons
(pihole / caddy / the auth provider). The only command that SSHes is `hemma deploy` — the deliberate
exception to the no-SSH non-goal (design §6.8/§12) — which fans out `hemma preflight` (no uncommitted
hemma-owned files there), a sync with origin, `git pull --ff-only` and then `hemma apply` across the fleet
(remotes first, self last); everything else never SSHes. Every host needs a `hemma` that knows `preflight`.
`design.md` is the authoritative design document; read it before non-trivial changes.
Section references in code comments (`design §6`, etc.) point at it.

The tool was named `sd`, then `splitdns`, and is now `hemma` (Swedish "at home") — it outgrew
"split DNS" once it also managed Caddy and auth-provider config. Released artifacts ship a
`splitdns` alias (brew/deb symlink), and both legacy generations of on-disk names (manifest,
generated filenames, Caddyfile import line, .gitignore markers) are auto-migrated — see
`legacyManifestNames`, `legacyImportLines`, and `legacyGiBlockStarts` in `internal/cli`.

## Build / test / release

Everyday commands are in the `Makefile` — `make help` lists them (`make check` = gofmt + vet + all tests;
`make test-fast` skips the ~40s CLI snapshot tests; `make snapshots-update` regenerates the goldens;
`make fixture-redeploy` recreates the UI test container). Underneath they are plain `go` commands:

```sh
go build ./...                       # build
go test ./...                        # all tests
go test ./internal/plan/             # one package
go test ./internal/cli/ -run TestFoo # one test
go run ./tools/genman                # regenerate man/hemma.1.gz from CLI help
```

Deployment to the hosts is via the apt package (Cloudsmith) published by the tag-triggered
release workflow; there is no deploy script in this repo.

Per the user's global convention: build and live-verify on the Mac before committing/pushing in this repo.

**Never build Go inside a Docker image.** The `hemma-ui` image (`Dockerfile.ui`) only COPIES `<platform>/hemma-ui` in from its build context (`ARG
TARGETPLATFORM`: `linux/amd64/hemma-ui`, `linux/arm64/hemma-ui`); cross-compile it with Go first — `make ui-binary` (`CGO_ENABLED=0 GOOS=linux
GOARCH=$(UI_ARCH) go build`, `UI_ARCH` defaults to arm64: the edge host and Colima on Apple silicon), which fills `dist/ui-context/`, the same folder
layout goreleaser builds for a release. `make ui-image` and `make fixture-redeploy` do that for you. A build stage in the image recompiles every
dependency each time and its cache once filled a whole Docker disk.

**Two releases, two tags, independent versions.** The `hemma` CLI is released by a `cli-v<X.Y.Z>` tag (`.github/workflows/release.yml`, `.goreleaser.yaml`:
GitHub release titled "cli vX.Y.Z", apt, Homebrew). That workflow also creates and pushes the bare `v<X.Y.Z>` on the same commit, because Go's module
proxy and `go install ...@vX.Y.Z` read only plain semver tags and the formula's download URLs are built from it; GoReleaser runs on the bare tag as
before. A tag pushed with the workflow's token starts no workflow, so it cannot release twice, and a bare `v*` pushed by hand releases nothing. The
`hemma-ui` image is released by a `ui-v<X.Y.Z>` tag (`.github/workflows/release-ui.yml`, `.goreleaser.ui.yaml`):
goreleaser cross-compiles both architectures on the runner, builds `Dockerfile.ui` for `linux/amd64,linux/arm64` (QEMU only for the image's own `RUN`
steps) and pushes `ghcr.io/miista/hemma-ui:<X.Y.Z>`, `:<X.Y>` and `:latest`. The workflow then makes a GitHub release on the `ui-v` tag itself, titled
"ui vX.Y.Z" with no files: the commit subjects since the previous `ui-v` tag (filtered like the CLI's changelog) and a compare link — goreleaser cannot
write that one, since the only tag it can parse is the bare version, which is not in git. Tagging is
manual, and **which tag(s) to push is the person's call: a change to the kernel (everything both are built from: `internal/cli/kernel`, `config`, `plan`,
`render`, `sync`, `manifest`, `auth`) means tag both; a UI-only change (`cmd/hemma-ui`, `Dockerfile.ui`) only `ui-v…`; a CLI-only change
(`internal/cli`, `main.go`) only `v…`.** Push both tags in one `git push` when both are due. The open-source goreleaser only accepts a plain semver tag,
so the image workflow strips the prefix (`ui-v0.3.0` → `v0.3.0`) and passes it as `GORELEASER_CURRENT_TAG` with `--skip=validate`; the CLI's goreleaser
config ignores `cli-*` and `ui-*` tags (`git.ignore_tags`) so its changelog never takes a prefixed tag as the previous release. A manual run of either
workflow is a dry run (it builds with the version you give; nothing is pushed); `.github/workflows/release-check.yml` builds the image for both platforms
on every pull request that touches how it is built. Older bare `vX.Y.Z` tags (up to the last release before `cli-`) stay as they are. The only credential is the workflow's own `GITHUB_TOKEN` with `packages: write`. The first image on the edge host was put there by hand
(`docker save | ssh edge docker load`, then `docker compose up -d hemma` in `~/docker/edge`); that still works for a local build.

## Architecture — the sync pipeline

Data flows one direction through six packages. The layering is deliberate; respect it.

```
services.yaml → config → plan → render        (desired state)
                          ↓   ↖ auth           (auth-provider boundary: access-control
                  sync (Engine) ←→ manifest      artifact, validation, credentials)
                          ↑                    (reconcile + safe GC)
                        cli                    (parse, mutate YAML, invoke sync)
```

- **`internal/config`** — loads/mutates/persists `services.yaml`, the sole source of truth. The tool
  **owns** this file and rewrites it wholesale; ordering and human comments are not preserved. Output-path
  segments live here as data (per-host overridable), not hardcoded.
- **`internal/plan`** — validates each service **independently**, collecting per-entry errors instead of
  stopping (partial success is the norm). Produces the desired set of `(path, content)` files. Also emits
  synthetic `@domain:` TLS-snippet owners — filter with `plan.IsSyntheticOwner`.
- **`internal/auth`** — the pluggable auth-provider boundary (`Provider` interface + registry, design
  §4.6): access-control artifact rendering, read-only config/users validation, credential minting, and
  the provider's apply commands. Providers return content; they never write files.
- **`internal/render`** — pure string generation for the two output types. All generated files carry the
  `# GENERATED by hemma — do not edit` header. Caddy ordering matters: TLS snippets must be defined
  before the site blocks that import them.
- **`internal/sync`** — the **Engine**: the ONLY code that writes or deletes generated files (single-writer
  invariant, design §6). `Incremental` mode writes/updates but never deletes; `Complete` mode also GCs
  orphaned manifest-tracked files. Diffs the plan against the manifest.
- **`internal/manifest`** — records which files each service generated (`hemma-manifest.yaml`). It is the
  **authority for safe deletion**: only manifest-tracked files are ever deleted. Unparseable → rebuilt from
  config. (`splitdns-manifest.yaml` and `sd-manifest.yaml` are the pre-rename names, auto-migrated
  on first load.)
- **`internal/cli`** — the front end: `Run`, the global `-C` flag, help, version, completion and `UsageText`;
  everything else is routed to the kernel via `kernel.Route`. A thin `main.go` just calls `cli.Run`.
- **`internal/cli/kernel`** — the implementation of every command (flags, work, printing, exit codes), shared by
  the CLI and `hemma-ui`. Commands still print, but into the `kernel.Env` they are given (`Out`/`Err` writers,
  `Ctx` bounding every external command, `NoTerminal`, `Self`) — never straight to `os.Stdout`/`os.Stderr`, and
  external tools start via `env.command` so they honour `Ctx`. add/update/remove mutate the YAML then
  call the shared sync engine — they contain **no file-writing logic of their own**.

**Snapshot tests** (`internal/cli/snapshot_test.go`, goldens in `internal/cli/testdata/snapshot/golden/`) run every
command in-process against an anonymized fixture and pin exit code, stdout, stderr, changed files (hashed) and the
sequence of external commands (fake docker/caddy/dig/ssh/git on `PATH`). After an intended behaviour change,
regenerate with `go test ./internal/cli/ -run TestSnapshots -update` and review the diff. The fixture is synthetic
(RFC 5737 addresses, `home.example`) — never copy a real `services.yaml` into `testdata`.

### Invariants to preserve when editing

- **Single writer**: only `internal/sync` writes/deletes generated files. Never write generated output from
  `cli` or elsewhere.
- **Validate before persist**: mutation commands (`cmdAdd` etc.) validate required flags, fqdn-matches-a-domain,
  and host-exists **before** touching the YAML, so a mistyped command never persists a half-formed entry.
- **`runSync` is the single sync path**, invoked as the tail of every mutation. Mode is chosen by mutation
  shape: add/update/enable use `Incremental` (can't orphan); remove-service and disable use the targeted
  `RemoveService` delete primitive (design §6.1); every host/domain/dns-host/auth mutation uses
  `Complete` (they can orphan files that must be GC'd, else `apply` refuses on drift).
- **Report-but-proceed**: per-entry validation failures are skipped and reported; the only globally fatal
  condition is YAML that won't parse. Exit non-zero if any entry was skipped (design §8).

## CLI conventions

- **Verb-first, `<verb> <noun> <args>`** across the whole CLI (`add service`, `remove host`) — one word order
  avoids add-service/service-add confusion. Routed via `dispatchNoun` in `cli.go`.
- Operates on `~/docker` by default; `-C`/`--chdir <dir>` overrides (git-style).
- Long + short flag aliases (`--fqdn`/`-f`); `version` verb; `--help` and `help <cmd>` per command.
- **`UsageText` in `cli.go` is the single source of truth for help** — `tools/genman` compiles the same
  strings into the man page, so `--help` and the man page can never drift. Edit help text there.
- Color/glyphs (`✓ ✗ ⚠`) are gated on stdout being a TTY and `NO_COLOR` unset, so piped/captured output stays plain.

### Naming: mechanism for fields, horizon for horizons

Two vocabularies, kept strictly apart — they drifted into each other once already
(top-level `public_paths` → `auth.bypass_paths`, then `public` → `tunnel`), each time
because one word was doing two jobs at different altitudes.

- **Field names describe the MECHANISM.** `tunnel` (is a cloudflared ingress entry
  generated?), `tunnel_dir`, `trusted_proxies`. A bool field asks a yes/no question, so
  every user-facing surface answers it that way: the editor confirms are `yes`/`no`, the
  `list` TUNNEL column is `yes`/`no`, the add-summary line is `tunnel: yes`. Do NOT label a
  bool field's values with horizon words — that was the state that made `public` ambiguous.
- **`local` / `public` mean a HORIZON, and only that**: where a request came from, or which
  horizon a name resolves on. They are the `auth.policy` keys and the language of the docs
  ("local horizon", "public horizon"). Never a field name.

So: `tunnel: true` (mechanism, bool) puts a service on the **public horizon** (vocabulary),
and `auth.policy.{local,public}` says what each horizon requires (§4.8: per-service, off by
default, resolved against `defaults.local_networks`; unset = `one_factor` on both, so it is purely
additive; a `bypass` rule carries no `subject` because Authelia forbids that combination). If a new field needs a
name, name the mechanism; if new prose needs to talk about reach, say local/public.

`auth.forward_gate` (§4.9) follows the rule: it names the mechanism (a forward-auth gate) and no horizon, since it is
only valid with `mode: oidc` + `tunnel: true` (plan refuses anything else). **Caddy asks the auth provider only on a
horizon where it has something to check** — `config.Service.GateScope` decides it, `render.CaddySite` renders it:
`GateAll` (`import auth`), `GateOutside` (`@hemma_outside not client_ip <local_networks>` + `route @hemma_outside {
import auth }` — the user's snippet is scoped, never rewritten) for an oidc service with the gate and for `forward` with a
local `bypass`, `GateNone` (plain proxy). Without `local_networks` a would-be `GateOutside` asks everyone. A gated oidc
service also gets forward-style `access_control` rules (local fixed at `bypass`, catch-all `policy.public` + groups,
bypass paths) next to its OIDC `authorization_policy`.

## Host-split commands (`apply`, `verify`)

`apply` and `verify` are **host-split** (the CLI; `hemma-ui` supplies its host from `deploy_host` instead, see below): they identify which host they run on via a local-IP match and
perform only the part they own. The DNS half (restart pihole / dig against it) runs only on the resolver;
the Caddy half (validate + reload) only on a host running Caddy; `apply` additionally has an auth half
(provider `ApplyCommands`: validate then restart the auth container) that runs only on the host running
the non-disabled `auth_service`. Neither command SSHes elsewhere — run on each host.
`apply` runs `caddy validate` **before** `caddy reload` so a missing/wrong cert aborts with a clear error
instead of failing mid-reload; it refuses if the repo has drift, and also if this host's Caddyfile
does not import `hemma.trusted_proxies.generated.caddy` (design §4.7 — hemma authors that trust
boundary, so it refuses to reload a Caddy that would silently ignore it; `doctor` only advises
there, since the global options block it belongs in is hand-maintained).

## `hemma-ui` — web UI (`cmd/hemma-ui`)

A deliberately small web UI (image: `Dockerfile.ui`; compose + synthetic test fixture:
`docker-compose.ui.yml`, `hack/ui-fixture/`). **It runs on the deploy host**, and Deploy is the kernel's
fleet `deploy` — the same code as the CLI, so it ssh's to the other hosts (ssh key + `known_hosts`
mounted read-only under `/data/.ssh`, see the compose file) and applies on this host last. There is no
hub/agent: that design (the unmerged `web-ui` branch) was dropped in favour of the shared kernel.
The container runs as uid 1000 (the image's `hemma` user: passwd entry for ssh, owned `/data/auth` for
authy). It is NOT a second implementation of hemma:

- **Every change goes through the kernel** (`commands.go` is an allowlist of typed commands, built as an argv,
  never a shell, and run in-process via `kernel.Route` with a capturing `kernel.Env`), so validation and the
  single-writer invariant are exactly the CLI's — it is literally the same code. The UI never writes
  `services.yaml` or generated files itself. `Env.NoTerminal` keeps the interactive editors from ever prompting,
  and the request's deadline is `Env.Ctx`, which kills hung external commands. The image contains only
  `hemma-ui`: there is no `hemma` binary here (`deploy`'s apply of this host runs in-process; the OTHER hosts
  run their installed `hemma apply` over ssh, as on the CLI).
- **The UI's host is `defaults.deploy_host`** (`host.go`; `main` hands `deployHostSelf` to `kernel.SetLocalHost`), not a guess from its
  network interfaces — it is required to run on the deploy host, so it takes that host's name from the file. This is the UI process
  only: the kernel and the CLI keep matching their own interfaces and still refuse `deploy` from any host but the deploy host. The
  kernel does NOT refuse when `deploy_host` is unset (it then runs anywhere), so the UI checks for itself: `deploy` and `apply`
  (`/api/deploy`, `/api/run`) answer 409 `noDeployHost` until it is set (`needDeployHost`), and the page switches Deploy, "Deploy right
  after saving" and Redeploy off with a pointer to Infrastructure → Settings → Deploy host (`needsDeployHost`). Editing,
  Verify and the rest stay available, so the setting can be fixed from the page. A deploy host that is not a declared host counts as unset.
- **Typed fields** (`Ref` in `commands.go`, validators in `refs.go`): a host is picked from hosts, a CIDR list is
  validated as CIDRs, a file is picked from the repo tree, a host's **SSH destination** is the `ssh` type (`[user@]host`, never
  starting with `-`: deploy runs `ssh <dest>`, so free text would be an option-injection hole). Every `defaults:` key is on the
  Infrastructure page's Settings, `deploy-host` included. The browser offers only valid choices *and* the
  server re-checks every value — the browser is a convenience, not the control.
- **Git is the change tracker.** Edits land in the repo immediately; the working tree is the "unsaved changes"
  list, `/api/diff` renders it as things (services, hosts, defaults — current vs changed), hemma's generated
  files are listed in a fold where each opens to its diff (`GET /api/filediff?path=`, read-only: it answers only for a
  path in the current changed-files list, and never reads a new file through a symlink), and other files are listed by
  name, never interpreted. With `services.yaml` unchanged but files re-rendered (a new hemma renders differently), the
  dialog says so instead of "No changes.". `apply`/`deploy` are refused
  server-side while the tree is dirty, so everything that goes live carries a commit message.
- **Two phases, deliberately separate:** *saving* (the warn bar at the bottom while there are unsaved changes: Discard —
  which asks inline, Keep / Discard N changes — and Review & save, which commits with a message) and *deploying*: an
  accent bar ("Saved, not live yet" + Deploy) after a save that was not deployed — remembered in `localStorage`, plus
  unpushed commits that change a file hemma owns (`ahead_hemma`; a commit to a compose file is ahead of origin too, but
  hemma has nothing to make live for it) — and **Tools → Recovery → Redeploy everything** (the Tools dialog), which is always there, so deploy still works
  on a freshly set-up machine with nothing to save (nothing about what is LIVE is ever inferred from the page). Deploy is the kernel's `deploy`, which first brings this
  checkout level with origin itself (`git pull --rebase`, aborted cleanly on a real conflict; then `git push`; then a
  drift check), so the UI has no push of its own and "origin moved" is never a dead end for the person. With no origin
  it just applies. **A deploy outlives its stream** (it runs detached from the request, `workContext`), and the stream is often
  cut: a deploy from outside restarts the very cloudflared it is streamed through. So the UI records each deploy
  (`lastdeploy.go`: id, running/ok, steps, output) in `HEMMA_UI_DATA/last-deploy.json` — on the data volume so it survives an
  update of hemma-ui; a record a stopped process left "running" is marked interrupted at startup — and `/api/state` reports it
  as `deploy`. The stream's first event carries the id. When the stream drops, the page says "Connection lost", stays
  "deploying" (no second Deploy offered) and checks the state until its deploy (by id) is finished, then shows the real
  outcome and, on success, clears "Saved, not live yet".
- **A file that cannot be loaded is still editable.** Every typed form needs a loadable `services.yaml`, so
  when it is not, `/api/state` reports why and the page shows a recovery screen: **Discard unsaved changes**
  (the usual cause is an unsaved edit) or **Edit services.yaml** as text. The text goes through
  `kernel.ReplaceConfig` — the UI still never writes the file itself — which refuses text that does not
  parse (nothing written) and otherwise writes it as given and syncs like any other mutation. The raw POST is
  a recovery only: while the file loads it answers 409, so the typed commands and their checks are the one
  way to change a healthy file (`TestRawConfig_ReadReplaceAndRefuse` pins it).
- **hemma must be able to read what it writes.** The none auth mode is the empty string internally and was
  written as `mode: ""`, which the reader refused — a service with mode none plus bypass paths/groups/policy
  made the whole file unloadable. The writer now spells it `none`; the reader still accepts the old `mode: ""`
  but refuses a mapping with NO mode key (so `auth: {groups: [x]}` never reads as "no auth"). The auth form
  only offers fields that apply to the chosen mode, and the server refuses a combination hemma would skip.
- **Not exposed on purpose:** credential minting (`create app/user`), and terminal-only `help`/`completion`/
  `version`/`defaults` listings, and `list` (the Services page is the overview). The file browser only ever shows
  the mounted repo (no `..`, no symlinks, no dot-folders). When `hemma doctor` finds nothing, Issues shows just
  "No issues"; the two checks you can run yourself (Verify, Measure) live in the Tools dialog. Verify's result is drawn like the deploy
  checklist (`verifyView`: a card per service, a ✓/✗ row per check, the outcome, the raw text under "Raw output"); it only reads the
  shape of `hemma verify`'s lines and falls back to the raw text when it finds no service. Measure is drawn the same way
  (`measureView`: a card per measured path — two with `--compare` — with a bar per timing, spread and range beneath, runs folded away).
- **Security posture:** sign-in is **authy** (`github.com/miista/authy`), the outermost layer — nothing,
  not even the page or `/static/`, is reachable without a session. Configure `HEMMA_UI_OIDC_*` (all four
  together; the IdP decides who may sign in) or, with none set, the first visitor creates a local account —
  so create it yourself right after starting. `HEMMA_UI_AUTH_DISABLED` is refused at startup (`newGate`):
  this process must never run ungated. Session state lives in `HEMMA_UI_DATA` (default `/data/auth`, a
  volume). Mutating requests also get a same-origin check. Still plain HTTP — put it behind TLS. The
  container mounts the Docker socket (root-equivalent on the host), so keep it LAN-only. Caveat: authy
  has not had an independent security review yet (its own README says so). `auth_test.go` pins that every
  route is refused without a session.
- **The Services page** groups services by host (collapsible, searchable with ⌘K / Ctrl+K); a row opens the service's
  form directly. Every row ends in two fixed icon columns (`svcMarks`), so they line up for scanning: **reach** (house = local only,
  globe = `tunnel`) and **sign-in**, which answers only "is it fronted by the auth provider?" (labels name the role, never the product: providers are pluggable): filled padlock = `forward`/`oidc`, outline
  padlock = `external`, key = the `auth_service` itself, red ⚠ = `none`. A bypass policy comes from the server as `auth-bypass`
  (resolved with `AuthPolicy.ForService`, never re-derived in the page): `local` is an amber filled padlock, `everywhere` is the red
  ⚠ like `none` (and a `doctor` advisory). A disabled service's icons stay muted — it is not served, so nothing to warn about.
  All of it comes from one function (`svcIcons`): the row, the **legend** (the ⓘ after the summary line, `#ldlg`) and the service
  form, which shows the same icon and its words under the Network and Authentication card titles and follows the form live. For
  that the form asks `GET /api/signin?mode=&tunnel=&local=&public=` (read-only, the same `authBypass` helper `/api/state` uses)
  whenever the mode, the tunnel or a policy changes; the latest answer wins and a failed ask leaves the icon as it was. The form is the existing typed form drawn as three cards (General, Network, Authentication) with
  Host and Auth mode as segmented choices; **Enabled** is a switch in it (not a field of `update service`: after the
  form's own command the page runs `enable`/`disable service`). History, Rename and Remove are buttons at the left of
  the footer (Remove still confirms). **On a phone** an edit form's footer is just **Close** with a chevron menu button (History, Rename,
  Remove; up while shut, turning down when open) to its right, and a floating **Cancel / Save** bar slides up above it once the form has changed (`setFloaty`):
  Cancel resets the form to how it opened, Close leaves it; Verify and the other run-forms keep their normal footer. **History** is a tab (`#/history`; `#/history/<service>` is the same list filtered to one service — the service dialog's
  History button opens that, and an "All history" button clears it; leaving the tab and coming back keeps the filter and the search): `GET /api/history` (`history.go`) walks the commits that
  touched `services.yaml` and compares each version with the one before (`diffEntities`, shared with the unsaved-changes diff), so
  an entry is a Save: its message, who, when, and everything it changed (services, hosts, domains, settings). With `?service=` it
  follows that one service back through a rename. Entries start folded (a single entry starts open). The search box matches the
  message, author, date, entity and field names and **both the old and the new values** (every word must match); it narrows the
  list and marks the matches, and does not open anything (what you opened stays open). A save that changed several things folds each change on its own (with Expand / Collapse all);
  a lone change just shows its table. On a service's own history the entry's `host/service:` prefix is dropped from the title (the full message
  stays as the tooltip and is still searched). Removed services show up as entries, so their history is reachable by searching for
  them. Recent saves are dated "Today, 22:31" / "Yesterday, …" and by weekday for the rest of the week; a week or more ago is a date (in the
  browser's language; the exact time is the tooltip, and both forms are searched). While it loads, a clock-face spinner (`spinner()`) sits in the middle of the page (the hand sweeps; still under `prefers-reduced-motion`). It is read a page at a time (30 saves, then a **Load older** button; one service's changes are read in chunks of 40 commits until 20 are
  found, the page carrying the name the service had at that point so a rename is followed across pages). Nothing is stored by hemma; it is all
  git. Unsaved changes are not in it, and there is no Restore
  (yet). A row's chip (Added / Changed / Removed) comes from the diff. Something removed but not saved
  (a service, host or domain) shows struck through with an **Undo**, which re-runs the matching `add …` with the values the
  diff recorded (`undoRemoved`), so no state lives only in the page. Host and domain rows open on a click; hemma cannot
  rename a domain, so a domain's dialog only shows it and offers Remove. There is no read-only details dialog any more.
- **Leaving a dialog with unsaved work asks first** (`leave()`): ×, a click on the backdrop, Esc and a phone's Close all show
  "Discard your changes?"; with nothing to lose they just close. Cancel (and a form's Cancel / Save bar) is the explicit way
  to throw edits away and does not ask. What counts as "dirty": a form's field values (`formDirty`) and the raw `services.yaml`
  editor's text. The **Save dialog never asks**: the message you were writing is kept in `localStorage` (`hemma.saveDraft`)
  and is back when you reopen it, until the changes are saved or discarded.
  When the unsaved changes concern exactly one service, the dialog shows "Will be prefixed with `<host>/<service>:`" above the
  box (`commitPrefix`) and prepends it to what is typed, so messages follow `<host>/<service>: <message>`; with that prefix the
  box asks the keyboard not to capitalise (`autocapitalize=none`). More than one entity, or anything but a service: no prefix.
- **Issues lists exactly what `hemma doctor` says.** `GET /api/issues` runs doctor read-only (no write lock, so it
  can run after every change to keep the tab's badge current) and returns its output verbatim; the page splits it
  into findings without altering a line. A **Fix** button runs `hemma doctor --fix` and appears only when doctor itself
  says `--fix` can help (its "Run 'hemma doctor --fix' …" hints; `doctorFixable` follows that wording and
  `TestIssues_…` pins it to the real output) — what `--fix` cannot resolve (advisories, known_hosts, …) is just
  listed. Successful changes are confirmed with a plain toast: the warnings a sync prints are repo-wide advisories,
  not about that change, and belong on Issues.
- **Four pages and a dialog:** Services (the list), Infrastructure (hosts, domains and — under them — the repo-wide Settings, the
  defaults), History (every Save), Issues (doctor). **Tools** (Verify, Measure, Recovery → Redeploy everything) is a dialog, not a page — rarely needed —
  opened from the burger in the phone's top bar or the Tools button in the sidebar's foot; choosing a tool closes the dialog first,
  so dialogs never stack, and Redeploy asks for its confirmation inside it. The burger menu also carries the version and Sign out.
  `#/settings` (Settings used to be a page) redirects to Infrastructure; the old `#/tools` lands on Services. The unsaved-change
  badge on a tab counts that page's entities (`diff.go` kinds: Service → Services, Host/Domain/Setting → Infrastructure).
- **A phone scrolls the page, not a box inside it** (`#main` only scrolls on a wide screen): iOS scrolls to the top when the status
  bar is tapped, but only for the page itself. So under 720px the header is `position: sticky`, and the tab bar, the bars and the toast
  are `position: fixed`; while a dialog is open the page behind it is locked (`overflow: hidden`). Use `scrollToTop()`, not
  `#main.scrollTop`.
- **Phones are the primary screen:** keep fields at 16px on every touch screen (`@media (pointer: coarse)`, so an iPhone in landscape
  and an iPad too — iOS zooms into smaller ones) and `touch-action: manipulation` everywhere (no double-tap zoom), the shell switches to the phone layout under 720px, and
  nothing may overflow sideways. Test against `hack/ui-fixture` (synthetic data; never a real host).
- **Look and feel:** the "direction c" design handoff (`form-redesign-request-*/` in the checkout, not committed; the latest numbered one wins):
  Instrument Sans + JetBrains Mono, a blue accent, a sidebar on a wide screen and a top bar + bottom tab bar under
  720px, bordered cards, soft change chips. It follows the system light/dark setting. All colours are tokens with a
  light and a dark value in `static/style.css` — never hard-code a colour in the page. The page (`static/index.html`)
  holds markup and script only; the stylesheet and the font files (SIL OFL-1.1, licenses alongside) are embedded and
  served from `/static/` behind the same login. Keep text at WCAG AA (4.5:1) in both themes. The brand mark
  (`static/icons/icon.svg`, plus `favicon.ico` as the fallback: 16/32/48 px PNGs packed by hand from the SVG — render them on a
  TRANSPARENT background, e.g. headless Chromium with `omitBackground`; a renderer that paints white leaves white corners around the
  rounded tile. The page links both with `?v=<file hash>` (`versionIcons`): browsers keep favicons in a store that ignores
  Cache-Control, so a changed icon only shows up under a new URL) is the tab icon AND the
  page's brand tile (`.brand-tile` is just the SVG as a background): an angular lowercase `h` leaning forward, cut from straight lines (a chamfered
  arch, a notch in the stem at the x-height, a sliced right foot), fading from white at the top to the accent blue at the feet. It is a fixed dark tile because an icon cannot follow the
  system theme, so it is the same in both. Being under `/static/` it is behind the login too (`auth_test.go` pins that).

## Scope boundary (do not cross)

**Update (commit `e49d8bb`, 2026-07-28): this section describes an architecture superseded in
code.** hemma no longer reads a hand-maintained compose file's tunnel labels — it now **writes**
each host's cloudflared `config.yml` directly (`internal/plan/plan.go`'s `planCloudflaredConfig`),
generated from every service's `tunnel` field the same way the Pi-hole/Caddy files are. `design.md`
§12 and this section still describe the pre-`e49d8bb` label-reading design and need a rewrite;
until that happens, treat the paragraphs below as **historical**, not current behavior, and prefer
`internal/plan/plan.go`, `internal/cli/list.go`, and `internal/cli/gitignore.go` (its `checkDrift`
call) as the source of truth.

`tunnel: true` on a service opts it into the public horizon; absent means local-only (renamed from
`public` in August 2026; the old key and `--public` flag are still accepted and migrated on load). hemma
generates a tunnel ingress entry for it in the relevant host's `config.yml` (path set by
`defaults.tunnel-dir`) alongside the DNS/Caddy files — same single-writer, same manifest-tracked
GC. There is no longer a separate read-only "PUBLIC" observation to reconcile against a
foreign file: `list`'s PUBLIC column is a direct echo of the declared field, and `doctor`'s
generic drift check (`checkDrift`) covers a missing/orphaned `config.yml` entry the same way it
covers any other generated file. The former **auth bypass** / **declared but not served** /
**undeclared exposure** checks (that read compose tunnel labels) were removed along with the
label-based design — there is no foreign file left to bypass or drift against.

**Never write a docker-compose.yml** is still absolute — that boundary is unaffected; the compose
file it used to read (for tunnel labels) is simply no longer read either, since hemma owns
`config.yml` outright instead.
