# hemma-ui fixture

A fully synthetic single-host homelab for exercising `hemma-ui` end to end — including
`apply` and `deploy` — with no real hosts, data or credentials.

```sh
./setup.sh                                  # builds .work/: repo + local bare remote + throwaway cert
docker compose -f compose.yml up -d --build
# UI: http://127.0.0.1:18080  (user admin, password "fixture")
docker compose -f compose.yml down -v       # tear down; re-run setup.sh for a fresh repo
```

**Reaching it from another device (e.g. a phone on your home network).** By default the UI is
loopback-only. The UI container has the Docker socket, so whoever can log in controls every
container on this machine — only widen it with a generated password, and bind to the one LAN
address rather than all interfaces:

```sh
# FIXTURE_BIND must be an address that exists where the container runs:
#   Docker Desktop:  the Mac's LAN IP        ($(ipconfig getifaddr en0))
#   Colima:          the VM's address        (the ADDRESS column of `colima list`; needs
#                    `network.address: true` in colima.yaml) — NOT the Mac's own IP
export FIXTURE_BIND=<that address>
docker compose -f compose.yml up -d --build
# then browse to http://$FIXTURE_BIND:18080 AT ONCE and create the local account (the first visitor
# creates it, and the container has the Docker socket) — plain HTTP, so use only on a trusted network
```

Once bound to a non-loopback address, the loopback URL stops working; use the same address on the Mac.

To reset to a fresh repo, tear down first (`down -v`), then `./setup.sh` again: the containers
bind-mount `.work/repo`, so re-creating it underneath running containers leaves them on the old one.

**Optional: auth-provider step.** By default there is no `auth_service`, so `apply` skips the
auth half. To exercise it too:

```sh
./setup.sh --with-auth
docker compose -f compose.yml --profile auth up -d --build
```

This declares an `authelia` service in `services.yaml` and starts a stub `authelia` container whose
fake `authelia config validate` succeeds, so `apply` runs validate-then-restart on it. Use the same
mode for both commands: `--with-auth` without `--profile auth` makes `apply` fail on the missing container.

| Container | What it is | Why |
|---|---|---|
| `caddy` | the real `caddy:2-alpine` | `caddy validate` / `caddy reload` genuinely run |
| `pihole`, `cloudflared` | `alpine sleep` stubs | `apply` only needs `docker restart` to succeed |
| `authelia` (optional) | `alpine` + a stub `authelia` script | only for `--with-auth`; answers `config validate` and `docker restart` |
| `hemma-ui-fixture` | `Dockerfile.ui` | fixed IP `172.28.0.10` = the host declared in `services.yaml` |

Not covered: `hemma verify` (it digs against a real pihole), and the auth provider unless you use
`--with-auth`. Requires ports/names `pihole`, `caddy`, `cloudflared`
and subnet `172.28.0.0/24` to be free.
