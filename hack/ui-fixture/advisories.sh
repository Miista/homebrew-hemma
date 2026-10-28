#!/usr/bin/env bash
# Puts a mock of (nearly) every finding `hemma doctor` can raise into the fixture repo, as ONE commit, so the UI's
# Issues page has all of them to show. Everything is synthetic (example domains, Alice/Bob, no real hosts).
#
#   ./advisories.sh           apply (refuses if the repo has unsaved changes)
#   ./advisories.sh reset     undo: reverts that commit and removes the files it left ignored
#
# It needs a fixture with defaults.auth_service set and an auth provider config on one host; it finds the hosts and the
# generated files it edits by looking, so it names none (and says so when the repo does not look like that).
# The imported fixture already raises two more kinds, so they are not added here: "the access policy on X is not enforced"
# and "OIDC logins to X will fail — no client is registered".
#
# What is mocked, and what deliberately is not (each pair would overlap or contradict):
#   .gitignore ........ generated files gitignored · hemma's block out of date · blanket data/ rules outside the block
#   Caddyfiles ........ on up to three hosts: the trusted-proxies import missing (the auth host) · a generated import missing ·
#                       an outdated one
#   drift ............. a generated file modified · missing · orphaned
#   auth, provider up . anyone can access X (bypass on both horizons) · a group that matches no user · nobody can access X ·
#                       access_control defined twice
# Not mocked: "access control is declared but not enforced" (the opposite of "defined twice": the file is not loaded at all),
# the three "could not verify / cross-check …" advisories (each replaces the check it stands in for), and the ones that need
# auth_service or auth_snippet unset or wrong (redirect loop, empty no-op snippet, not a defined service, nothing protected,
# OIDC unverifiable): they contradict the state the rest of the auth mocks need. An unreadable auth_snippet would re-render
# half the repo as drift. The deploy ssh-readiness finding needs no mock: the fixture's remote hosts already trigger it.
set -euo pipefail

cmd="${1:-apply}"
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work="${FIXTURE_WORK:-$here/.work}"
repo="$work/repo"
marker=".fixture-advisories"
subject="fixture: mock advisories"

die() { echo "advisories.sh: $*" >&2; exit 1; }
g() { git -C "$repo" -c user.name=fixture -c user.email=fixture@example.com "$@"; }

[ -d "$repo/.git" ] || die "no fixture repo at $repo — run ./setup.sh first"
# The UI offers Deploy for a commit that is ahead of origin and changes files hemma owns, and a deploy would push the mocks and run
# `apply` on the fixture's stack. So the commit goes to the fixture's own bare remote straight away (the repo's origin is the
# container's path /remote.git, which the host cannot use): nothing is applied, the UI just has nothing "ahead" to deploy.
push_to_remote() {
  [ -d "$work/remote.git" ] || return 0
  g push -q "$work/remote.git" HEAD:main && g update-ref refs/remotes/origin/main HEAD
}
[ -z "$(g status --porcelain)" ] || die "the fixture repo has unsaved changes — save or discard them in the UI first"

case "$cmd" in
reset)
  [ -f "$repo/$marker" ] || die "the mock advisories are not applied"
  sha=$(g log -1 --format=%H --grep="^$subject\$")
  [ -n "$sha" ] || die "cannot find the '$subject' commit"
  g revert --no-edit "$sha" >/dev/null || die "the revert conflicts (later commits touch the same files): git -C $repo revert --abort"
  # what the commit left behind: the generated files of the mock services, which its .gitignore hid from git
  rm -f "$repo"/*/caddy/data/sites/mock-*.caddy
  push_to_remote
  echo "reset: reverted $sha"
  exit 0
  ;;
apply) ;;
*) die "usage: $0 [apply|reset]" ;;
esac

[ ! -f "$repo/$marker" ] || die "already applied — './advisories.sh reset' first"
grep -q '^    auth_service:' "$repo/services.yaml" || die "defaults.auth_service is not set — use an imported fixture (make fixture-import) or ./setup.sh --with-auth"
# The host that runs the auth provider: the one directory holding its config.
authcfgs=("$repo"/*/authelia/data/config/configuration.yml)
[ ${#authcfgs[@]} -eq 1 ] && [ -f "${authcfgs[0]}" ] || die "expected the auth provider config on exactly one host: $repo/<host>/authelia/data/config/configuration.yml"
authcfg="${authcfgs[0]}"
AUTH_HOST=$(basename "$(dirname "$(dirname "$(dirname "$(dirname "$authcfg")")")")")
# the other hosts that run Caddy, in a stable order
others=()
for d in "$repo"/*/caddy/data; do h=$(basename "$(dirname "$(dirname "$d")")"); [ "$h" = "$AUTH_HOST" ] || others+=("$h"); done

(cd "$root" && go build -o "$work/hemma" .)
hemma="$work/hemma"

# ── .gitignore: three findings ──────────────────────────────────────────────────────────────────────────────────
# Written FIRST: the rule hides the generated files of the mock services below, which is what "gitignored" needs.
cat > "$repo/.gitignore" <<'EOF'
# someone's own rule, broad enough to catch hemma's generated sites for the mock services
**/sites/mock-*.caddy

# an older hemma's block: it re-included whole data/ directories (this version writes narrower rules)
# >>> hemma managed >>>
!**/data/
!**/data/**/
# <<< hemma managed <<<

# and the predecessor tool's blanket rule, left outside the block
!**/data/
EOF

# ── services for the auth mocks ─────────────────────────────────────────────────────────────────────────────────
add() { "$hemma" -C "$repo" add service "$@" >/dev/null 2>&1 || true; }
# policy bypass on both horizons: the padlock of a provider-backed service, the access of mode none
add mock-open   --fqdn mock-open.example.com   --host "$AUTH_HOST" --backend mock-open:80   --auth-mode forward --auth-policy-local bypass
# one group that matches no user next to one that does
add mock-typo   --fqdn mock-typo.example.com   --host "$AUTH_HOST" --backend mock-typo:80   --auth-mode forward --auth-groups admins,mock-typo-group
# every group matches no user: nobody can get in (and the group is flagged as a typo too)
add mock-locked --fqdn mock-locked.example.com --host "$AUTH_HOST" --backend mock-locked:80 --auth-mode forward --auth-groups mock-nobody

# The users database the cross-checks read (only groups are read). It covers every group the fixture already uses,
# so the only "matches no user" findings are the mock ones above.
cat > "$(dirname "$authcfg")/users_database.yml" <<'EOF'
users:
  alice:
    displayname: Alice Example
    groups: [admins, media-admins, media-users, family, observers]
  bob:
    displayname: Bob Example
    groups: [media-users, family]
EOF

# access_control left in the provider's own config while the generated file defines one too: one silently wins
cat >> "$authcfg" <<'EOF'

access_control:
  default_policy: deny
  rules: []
EOF

# ── Caddyfiles: one finding per host ────────────────────────────────────────────────────────────────────────────
# the auth host: the trusted-proxies import is gone
cat > "$repo/$AUTH_HOST/caddy/data/Caddyfile" <<'EOF'
{
	auto_https off
}
import hemma.generated.caddy
EOF
# the next host: an outdated generated import
if [ ${#others[@]} -ge 1 ]; then
cat > "$repo/${others[0]}/caddy/data/Caddyfile" <<'EOF'
{
	servers {
		import hemma.trusted_proxies.generated.caddy
	}
}
import splitdns.generated.caddy
EOF
fi
# the one after: no generated import at all
if [ ${#others[@]} -ge 2 ]; then
cat > "$repo/${others[1]}/caddy/data/Caddyfile" <<'EOF'
{
	servers {
		import hemma.trusted_proxies.generated.caddy
	}
}
# (the generated sites are never imported)
EOF
fi

# ── drift: modified, missing, orphaned ──────────────────────────────────────────────────────────────────────────
sites="$repo/$AUTH_HOST/caddy/data/sites"
gen=()
for f in "$sites"/*.caddy; do case "$(basename "$f")" in mock-*) ;; *) gen+=("$f") ;; esac; done
[ ${#gen[@]} -ge 2 ] || die "the drift mocks need two generated site files in $sites"
echo "# edited by hand, not by hemma" >> "${gen[0]}"                              # modified
rm "${gen[1]}"                                                                   # missing
printf '# GENERATED by hemma — do not edit\nretired.example.com {\n\trespond "gone" 410\n}\n' > "$sites/retired-app.caddy"
printf 'retired-app:\n    - %s/caddy/data/sites/retired-app.caddy\n' "$AUTH_HOST" >> "$repo/hemma-manifest.yaml"   # orphaned: tracked, no longer wanted

cat > "$repo/$marker" <<'EOF'
Mock advisories for the UI's Issues page (hack/ui-fixture/advisories.sh). `./advisories.sh reset` removes them.
EOF
g add -A
g commit -q -m "$subject"
push_to_remote
echo "applied. 'hemma doctor' now says:"
echo
"$hemma" -C "$repo" doctor || true
