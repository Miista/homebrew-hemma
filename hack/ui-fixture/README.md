# hemma-ui fixture

A fully synthetic single-host homelab for exercising `hemma-ui` end to end — including
`apply` and `deploy` — with no real hosts, data or credentials.

```sh
./setup.sh                                  # builds .work/: repo + local bare remote + throwaway cert
docker compose -f compose.yml up -d --build
# UI: http://127.0.0.1:18080  (user admin, password "fixture")
docker compose -f compose.yml down -v       # tear down; re-run setup.sh for a fresh repo
```

| Container | What it is | Why |
|---|---|---|
| `caddy` | the real `caddy:2-alpine` | `caddy validate` / `caddy reload` genuinely run |
| `pihole`, `cloudflared` | `alpine sleep` stubs | `apply` only needs `docker restart` to succeed |
| `hemma-ui-fixture` | `Dockerfile.ui` | fixed IP `172.28.0.10` = the host declared in `services.yaml` |

Not covered: the auth provider (no `auth_service` configured) and `hemma verify`
(it digs against a real pihole). Requires ports/names `pihole`, `caddy`, `cloudflared`
and subnet `172.28.0.0/24` to be free.
