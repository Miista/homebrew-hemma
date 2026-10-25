package kernel

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/config"
)

// cmdList prints the declared inventory — hosts, domains, services. It is plain
// inventory, not a status view: no per-service validity checks. The one thing
// it does flag is the global can't-sync blocker (no dns_host), as a prominent
// warning BEFORE the inventory. Read-only.
//
// By default the Services section is filtered to those running on THIS host
// (matched by local IP); --all shows every service regardless of host.
func cmdList(env *Env, cfgPath string, args []string) int {
	cfg, code := loadExisting(env, cfgPath, "list")
	if cfg == nil {
		return code
	}

	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	all := fs.Bool("all", false, "show services on every host, not just this one")
	fs.BoolVar(all, "a", false, "alias for --all")
	jsonOut := fs.Bool("json", false, "stable JSON service listing for scripts (implies --all)")
	policy := fs.Bool("policy", false, "add LOCAL and PUBLIC columns: the per-horizon auth policy (design §4.8)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *jsonOut {
		return cmdListJSON(env, cfg)
	}

	// Which host are we on? If we can't tell (IP matches no host), fall back to
	// showing everything rather than an empty list.
	self := localHost(cfg)
	filtered := !*all && self != ""

	// Big fat warning first: without a dns_host, nothing can sync.
	if cfg.Defaults.DNSHost == "" {
		fmt.Fprintln(env.Out, warn+" No dns_host set — services cannot sync.")
		fmt.Fprintln(env.Out, "   Set the resolver host:  hemma defaults set dns-host <name>")
		fmt.Fprintln(env.Out)
	}

	fmt.Fprintf(env.Out, "%s== Hosts (%d) ==%s\n", boldOn, len(cfg.Hosts), boldOff)
	for _, name := range sortedKeysOf(cfg.Hosts) {
		var roles []string
		if name == cfg.Defaults.DNSHost {
			roles = append(roles, "dns_host")
		}
		if name == cfg.Defaults.DeployHost {
			roles = append(roles, "deploy_host")
		}
		marker := ""
		if len(roles) > 0 {
			marker = "  (" + strings.Join(roles, ", ") + ")"
		}
		fmt.Fprintf(env.Out, "  %-12s %s%s\n", name, cfg.Hosts[name].IP, marker)
	}

	fmt.Fprintf(env.Out, "\n%s== Domains (%d) ==%s\n", boldOn, len(cfg.Domains), boldOff)
	for _, name := range sortedKeysOf(cfg.Domains) {
		fmt.Fprintf(env.Out, "  %s\n", name)
	}

	// Auth is repo-wide config, not per-host — its own section, only shown when
	// something is configured (keeps a no-auth repo's output clean). The snippet
	// is any Caddy auth directive (forward_auth, basic_auth, …); hemma is
	// agnostic to its contents, so the section is named for the mechanism.
	if cfg.Defaults.AuthSnippet != "" || cfg.Defaults.AuthService != "" {
		fmt.Fprintf(env.Out, "\n%s== Auth ==%s\n", boldOn, boldOff)
		if cfg.Defaults.AuthSnippet != "" {
			fmt.Fprintf(env.Out, "  snippet:  %s\n", cfg.Defaults.AuthSnippet)
		} else {
			fmt.Fprintln(env.Out, "  snippet:  (none — set with 'hemma defaults set auth-snippet <path>')")
		}
		if cfg.Defaults.AuthService != "" {
			fmt.Fprintf(env.Out, "  service:  %s\n", cfg.Defaults.AuthService)
		} else {
			fmt.Fprintln(env.Out, "  service:  (none — set with 'hemma defaults set auth-service <name>')")
		}
	}

	// Gather the services to show, applying the this-host filter unless --all.
	var svcNames []string
	for _, name := range sortedKeysOf(cfg.Services) {
		if filtered && cfg.Services[name].Host != self {
			continue
		}
		svcNames = append(svcNames, name)
	}

	if filtered {
		fmt.Fprintf(env.Out, "\n%s== Services on %s (%d of %d) ==%s\n", boldOn, self, len(svcNames), len(cfg.Services), boldOff)
	} else {
		fmt.Fprintf(env.Out, "\n%s== Services (%d) ==%s\n", boldOn, len(svcNames), boldOff)
	}
	printServiceTable(env, cfg, svcNames, *policy)
	if filtered && len(svcNames) < len(cfg.Services) {
		fmt.Fprintf(env.Out, "  (%d on other hosts hidden — use --all to show)\n", len(cfg.Services)-len(svcNames))
	}

	repoRoot := filepath.Dir(cfgPath)

	// Auth-groups picture: union of the users database and services.yaml
	// groups (hidden when no group exists on either side).
	printGroupsSection(env, repoRoot, cfg)

	printAdvisories(env, repoRoot, authConfigWarnings(repoRoot, cfg))

	reportDrift(env, detectDrift(repoRoot, cfg, loadManifest(env, repoRoot, cfg)))
	return 0
}

// printServiceTable renders the services as an aligned table with an AUTH
// column showing the auth MODE (forward/oidc/external/-, or (portal) for the
// service that IS the auth provider) and a TUNNEL column echoing the declared
// `tunnel` field. This is a direct echo, not an observation, because hemma is
// now the sole author of the tunnel's ingress config (config.yml, per host) —
// there is no longer a separate "what does the tunnel actually serve" question
// to reconcile against the declaration, the way there was when a hand-
// maintained compose file could drift from services.yaml. Column widths are
// computed from the data (including headers) so it stays aligned regardless
// of name/fqdn lengths. Disabled services are marked in a trailing note
// column. When nothing is selected, prints a placeholder.
//
// With policy set, LOCAL and PUBLIC columns (the per-horizon auth policy,
// design §4.8) sit between AUTH and TUNNEL.
func printServiceTable(env *Env, cfg *config.Config, svcNames []string, policy bool) {
	if len(svcNames) == 0 {
		fmt.Fprintln(env.Out, "  (none)")
		return
	}
	type row struct{ name, fqdn, host, backend, auth, local, public, tunnel, note string }
	rows := make([]row, 0, len(svcNames))
	anyAuth := false
	for _, name := range svcNames {
		svc := cfg.Services[name]
		auth := "-"
		if svc.Auth.Mode != config.AuthNone {
			auth = string(svc.Auth.Mode)
			anyAuth = true
		}
		// The auth_service is the one row where "-" actively misinforms: it is
		// the login portal, so it looks unprotected while being the thing doing
		// the protecting. No auth MODE fits it — forward is refused (gating the
		// portal loops), oidc means "an Authelia client" when it IS Authelia,
		// and external means "not Authelia", the exact opposite. So the role is
		// shown instead of inventing a mode for it, parenthesised like the
		// (dns_host) marker in the Hosts section above. A mode set anyway (an
		// odd but representable state) is kept and the marker appended, so the
		// declaration is never hidden.
		if name == cfg.Defaults.AuthService {
			if svc.Auth.Mode == config.AuthNone {
				auth = "(portal)"
			} else {
				auth += " (portal)"
			}
			anyAuth = true
		}
		note := ""
		if svc.Disabled {
			note = "[disabled]"
		}
		tunnel := "no"
		if svc.Tunnel {
			tunnel = "yes"
		}
		local, public := horizonPolicies(svc)
		rows = append(rows, row{name, svc.FQDN, svc.Host, svc.Backend, auth, local, public, tunnel, note})
	}

	// Header + width computation. AUTH holds a mode, "-", or "(portal)"; TUNNEL
	// holds "yes"/"no" and is dropped entirely when disabled.
	hName, hFQDN, hHost, hBack, hAuth, hPub := "NAME", "FQDN", "HOST", "BACKEND", "AUTH", "TUNNEL"
	hLocal, hPublic := "LOCAL", "PUBLIC"
	wName, wFQDN, wHost, wBack, wAuth := len(hName), len(hFQDN), len(hHost), len(hBack), len(hAuth)
	wLocal, wPublic := len(hLocal), len(hPublic)
	for _, r := range rows {
		wName = max(wName, len(r.name))
		wFQDN = max(wFQDN, len(r.fqdn))
		wHost = max(wHost, len(r.host))
		wBack = max(wBack, len(r.backend))
		wAuth = max(wAuth, len(r.auth))
		wLocal = max(wLocal, len(r.local))
		wPublic = max(wPublic, len(r.public))
	}

	head := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  ", wName, hName, wFQDN, hFQDN, wHost, hHost, wBack, hBack, wAuth, hAuth)
	if policy {
		head += fmt.Sprintf("%-*s  %-*s  ", wLocal, hLocal, wPublic, hPublic)
	}
	fmt.Fprintf(env.Out, "  %s%s%s%s\n", boldOn, head, hPub, boldOff)
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %-*s  ",
			wName, r.name, wFQDN, r.fqdn, wHost, r.host, wBack, r.backend, wAuth, r.auth)
		if policy {
			line += fmt.Sprintf("%-*s  %-*s  ", wLocal, r.local, wPublic, r.public)
		}
		line += r.tunnel
		if r.note != "" {
			line += "  " + r.note
		}
		fmt.Fprintln(env.Out, line)
	}
	if anyAuth {
		fmt.Fprintln(env.Out, "  (AUTH: forward = imports the (auth) snippet; oidc = Authelia, app does OIDC itself; external = authenticated outside Authelia (app login / token / edge); (portal) = this service IS the auth provider; change with 'hemma update service <name> --auth-mode <mode>')")
	}
	if policy {
		fmt.Fprintln(env.Out, "  (LOCAL/PUBLIC: the auth.policy each horizon requires — bypass, one_factor, two_factor or deny; unset = one_factor; - = no policy, since only 'forward' and 'oidc' carry one (oidc has no bypass). PUBLIC is shown even when TUNNEL is no: the policy is declared, it just never fires from the internet)")
	}
	fmt.Fprintln(env.Out, "  (TUNNEL: yes = a cloudflared tunnel ingress entry is generated for the FQDN, so the internet can reach it; no = local horizon only)")
}

// horizonPolicies returns a service's effective auth policy per horizon, or
// "-" for both when its mode carries none: only `forward` and `oidc` take a policy
// (design §4.8). The public level is reported even for a tunnel-less service —
// the two axes are separate, so a declared public policy is shown, not hidden.
func horizonPolicies(svc config.Service) (local, public string) {
	if !svc.Auth.Mode.UsesProvider() {
		return "-", "-"
	}
	pol := svc.Auth.Policy.ForService(svc.Tunnel)
	return string(pol.ResolveLocal()), string(pol.ResolvePublic())
}

// jsonService is the stable shape emitted by --json: field names are the
// contract, not position or count, so new fields can be added later without
// breaking existing consumers (unlike a positional format). Always shows
// every service on every host — the --all filter is a human-table concept,
// a script consumer wants the whole inventory regardless of which host it
// runs on.
type jsonService struct {
	Name    string `json:"name"`
	FQDN    string `json:"fqdn"`
	Host    string `json:"host"`
	Backend string `json:"backend"`
	// AuthMode is the config.AuthMode value ("none" when ungated). The two
	// policy fields are the effective per-horizon levels and are omitted
	// unless the mode is `forward` or `oidc`, the ones that carry a policy.
	AuthMode     string `json:"auth_mode"`
	PolicyLocal  string `json:"policy_local,omitempty"`
	PolicyPublic string `json:"policy_public,omitempty"`
	Tunnel       bool   `json:"tunnel"`
}

// cmdListJSON prints every service as a JSON array, one object per service,
// sorted by name. No headers, no color, no advisories — just the array.
func cmdListJSON(env *Env, cfg *config.Config) int {
	names := sortedKeysOf(cfg.Services)
	out := make([]jsonService, 0, len(names))
	for _, name := range names {
		svc := cfg.Services[name]
		js := jsonService{
			Name:     name,
			FQDN:     svc.FQDN,
			Host:     svc.Host,
			Backend:  svc.Backend,
			AuthMode: string(svc.Auth.Mode),
			Tunnel:   svc.Tunnel,
		}
		if svc.Auth.Mode == config.AuthNone {
			js.AuthMode = "none" // AuthNone is "" internally; scripts get the documented word
		}
		if svc.Auth.Mode.UsesProvider() {
			js.PolicyLocal, js.PolicyPublic = horizonPolicies(svc)
		}
		out = append(out, js)
	}
	enc := json.NewEncoder(env.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(env.Err, "Error: encoding JSON: %v\n", err)
		return 1
	}
	return 0
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
