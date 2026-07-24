package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
)

// Read-only doctor checks on the PUBLIC horizon (design §12). hemma never
// writes docker-compose.yml, so none of these is --fix-able: each advisory
// carries the exact label snippet and where to paste it, in the same
// instructive style as the auth-config advisories.
//
// Three checks, in descending severity:
//
//  1. AUTH BYPASS — a forward-auth service whose tunnel ingress points DIRECT
//     at the container. The tunnel never traverses Caddy, so the (auth) snippet
//     never runs and the service is publicly reachable with no authentication
//     at all. This is the only check here that reports a live security hole.
//  2. DECLARED vs OBSERVED — the service declares `public: true|false` and the
//     compose label says otherwise. Undeclared services are silent: intent is
//     opt-in, so existing repos gain no advisories until they say what they want.
//  3. ORPHAN INGRESS — a hostname served publicly in a managed domain with no
//     services.yaml entry, so it has NO internal horizon. Internally the name
//     resolves via public DNS, leaves the LAN, and hairpins back through the
//     tunnel. It works, which is why it goes unnoticed.
//
// Checks 1 and 2 count as doctor problems (non-zero exit): one is a security
// hole, the other an explicit declaration being violated. Check 3 does not —
// nothing was declared, and the hostname is not hemma's to own.

// publicHorizonWarnings runs the three public-horizon checks. Silent when
// public-horizon reporting is off, and per-host silent when that host's compose
// file cannot be read (an unreadable file is not evidence of anything).
func publicHorizonWarnings(repoRoot string, cfg *config.Config) (advs []auth.Advisory, problems int) {
	pub := newPublicLookup(repoRoot, cfg)
	if !pub.enabled() {
		return nil, 0
	}

	// Services this check considers: enabled ones only. A disabled service
	// generates no Caddy block, so its label state says nothing about hemma.
	names := make([]string, 0, len(cfg.Services))
	for name, svc := range cfg.Services {
		if !svc.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		svc := cfg.Services[name]
		set := pub.hostIngress(cfg, svc.Host)
		if set == nil {
			continue // compose unreadable — the PUBLIC column already shows "?"
		}
		composePath := filepath.Join(repoRoot, cfg.Hosts[svc.Host].ResolvedDir(svc.Host), composeFile)
		in, served := set[strings.ToLower(svc.FQDN)]

		// --- 1. auth bypass ---
		if a, hit := authBypassAdvisory(cfg, name, svc, in, served, composePath, pub.proxyLabel); hit {
			advs = append(advs, a)
			problems++
		}

		// --- 2. declared vs observed ---
		if a, hit := declaredPublicAdvisory(cfg, name, svc, served, composePath, pub); hit {
			advs = append(advs, a)
			problems++
		}
	}

	// --- 3. orphan ingress (per host, not per service) ---
	advs = append(advs, orphanIngressAdvisories(repoRoot, cfg, pub)...)
	return advs, problems
}

// authBypassAdvisory reports a forward-auth service served DIRECT from the
// tunnel. Only mode forward is affected: an oidc service authenticates in the
// app itself, so reaching it without traversing Caddy is by design, and a
// no-auth service has no gate to bypass.
//
// The auth provider's own service is exempt, and not because it "needs to be
// reachable": `auth: forward` on the auth_service is REFUSED by the planner
// (protecting the portal would create a redirect loop), so that service is
// skipped and no site block — hence no gate — is generated for it. Warning that
// the tunnel bypasses a gate that was never rendered would be a false positive.
// services.yaml can still hold the combination, since the refusal happens at
// plan time rather than on persist, which is exactly why this guard is needed.
func authBypassAdvisory(cfg *config.Config, name string, svc config.Service, in ingress, served bool, composePath, proxyLabel string) (auth.Advisory, bool) {
	if !served || in.Proxied || proxyLabel == "" {
		return auth.Advisory{}, false
	}
	if svc.Auth.Mode != config.AuthForward {
		return auth.Advisory{}, false
	}
	if name == cfg.Defaults.AuthService {
		return auth.Advisory{}, false
	}
	return auth.Advisory{
		Headline: fmt.Sprintf("%s is publicly reachable WITHOUT auth — the tunnel bypasses Caddy", name),
		Body: []string{
			fmt.Sprintf("%s has auth mode forward, so hemma gated it by importing the (auth) snippet", name),
			fmt.Sprintf("into its Caddy site block. But %s serves it straight at the container,", composePath),
			"so public requests never traverse Caddy and the gate never runs.",
			fmt.Sprintf("Anyone on the internet can reach %s unauthenticated.", svc.FQDN),
		},
		Fix: []string{
			fmt.Sprintf("route the tunnel through Caddy — add to the %q service's labels:", in.Container),
			fmt.Sprintf("  %s: \"https://caddy:443\"", proxyLabel),
			"(or, if it is meant to be public and unauthenticated, clear the auth mode:",
			fmt.Sprintf(" hemma update service %s --auth-mode none)", name),
		},
		Then: "docker restart cloudflared",
	}, true
}

// declaredPublicAdvisory compares a DECLARED public horizon against the observed
// compose label. Undeclared services return no advisory: with no stated intent
// there is nothing to contradict.
func declaredPublicAdvisory(cfg *config.Config, name string, svc config.Service, served bool, composePath string, pub *publicLookup) (auth.Advisory, bool) {
	want, declared := svc.DeclaredPublic()
	if !declared || want == served {
		return auth.Advisory{}, false
	}
	if want {
		// Declared public, not labelled — the §12 gotcha, made visible.
		return auth.Advisory{
			Headline: fmt.Sprintf("%s declares public: true but has no public ingress", name),
			Body: []string{
				fmt.Sprintf("no %s label in %s names %s,", pub.label, composePath, svc.FQDN),
				"so the tunnel does not serve it and the name has no public DNS record.",
				"Internally it resolves (hemma generated that); from the internet it does not.",
			},
			Fix:  publicLabelSnippet(cfg, name, svc, pub),
			Then: "docker restart cloudflared",
		}, true
	}
	// Declared internal-only, but labelled — the security-relevant direction.
	return auth.Advisory{
		Headline: fmt.Sprintf("%s declares public: false but IS exposed to the internet", name),
		Body: []string{
			fmt.Sprintf("%s carries a %s label for %s,", composePath, pub.label, svc.FQDN),
			"which the tunnel serves and publishes a public DNS record for.",
			"The declaration says this service should be reachable on the LAN only.",
		},
		Fix: []string{
			fmt.Sprintf("remove the %s label from %s in %s,", pub.label, svc.FQDN, composePath),
			fmt.Sprintf("or, if the exposure is intended, declare it: public: true on %s", name),
		},
		Then: "docker restart cloudflared",
	}, true
}

// publicLabelSnippet builds the paste-in label block for a service that should
// be public. It is AUTH-AWARE, which is the whole point: a forward-auth service
// routed direct at its container would be publicly reachable with the auth gate
// bypassed (see authBypassAdvisory), so its snippet must route through Caddy.
// An oidc or no-auth service goes direct, with the port hemma already knows
// from `backend`.
func publicLabelSnippet(cfg *config.Config, name string, svc config.Service, pub *publicLookup) []string {
	container, port := containerAndPort(svc.Backend)
	viaCaddy := svc.Auth.Mode == config.AuthForward && name != cfg.Defaults.AuthService

	fix := []string{fmt.Sprintf("add to the %q service's labels in that compose file:", container)}
	if viaCaddy || port == "" {
		fix = append(fix, fmt.Sprintf("  %s: %q", pub.label, svc.FQDN))
	} else {
		fix = append(fix, fmt.Sprintf("  %s: \"%s:%s\"", pub.label, svc.FQDN, port))
	}
	if viaCaddy {
		fix = append(fix,
			fmt.Sprintf("  %s: \"https://caddy:443\"", pub.proxyLabel),
			"(routed through Caddy because auth mode is forward — direct ingress would")
		fix = append(fix, " bypass the (auth) gate entirely)")
	}
	return fix
}

// containerAndPort splits a `name:port` backend into its parts. A backend
// without a port (or an absolute host like host.docker.internal:8080) still
// yields a usable container guess.
func containerAndPort(backend string) (container, port string) {
	if c, p, ok := strings.Cut(backend, ":"); ok {
		return c, p
	}
	return backend, ""
}

// orphanIngressAdvisories reports hostnames served publicly, in a domain hemma
// manages, that have no services.yaml entry — so hemma generated no internal
// horizon for them. Scoped to managed domains on purpose: a homelab compose file
// legitimately serves names in other zones, and warning about those would be
// noise about something outside hemma's remit.
//
// One advisory per host, listing every orphan, rather than one per hostname:
// these are usually discovered in batches (a whole compose file predating hemma)
// and N separate advisories would bury the rest of doctor's output.
func orphanIngressAdvisories(repoRoot string, cfg *config.Config, pub *publicLookup) []auth.Advisory {
	declared := map[string]bool{}
	for _, svc := range cfg.Services {
		declared[strings.ToLower(svc.FQDN)] = true
	}

	var advs []auth.Advisory
	for _, host := range sortedKeysOf(cfg.Hosts) {
		set := pub.hostIngress(cfg, host)
		if set == nil {
			continue
		}
		var orphans []string
		for h := range set {
			if declared[h] || !inManagedDomain(cfg, h) {
				continue
			}
			orphans = append(orphans, h)
		}
		if len(orphans) == 0 {
			continue
		}
		sort.Strings(orphans)

		composePath := filepath.Join(repoRoot, cfg.Hosts[host].ResolvedDir(host), composeFile)
		body := []string{
			fmt.Sprintf("%s serves these publicly, but no service declares them:", composePath),
		}
		for _, h := range orphans {
			body = append(body, "  "+h)
		}
		body = append(body,
			"They have no internal horizon, so on the LAN they resolve via PUBLIC DNS —",
			"traffic leaves the network and hairpins back through the tunnel.")

		fix := make([]string, 0, len(orphans))
		for _, h := range orphans {
			in := set[h]
			backend := in.Container
			if in.Port != "" {
				backend += ":" + in.Port
			} else {
				backend += ":<port>"
			}
			fix = append(fix, fmt.Sprintf("hemma add service %s --fqdn %s --host %s --backend %s",
				suggestName(h), h, host, backend))
		}
		verb := "have"
		if len(orphans) == 1 {
			verb = "has"
		}
		advs = append(advs, auth.Advisory{
			Headline: fmt.Sprintf("%d public %s on %s %s no internal horizon",
				len(orphans), plural(len(orphans), "hostname"), host, verb),
			Body: body,
			Fix:  fix,
		})
	}
	return advs
}

// inManagedDomain reports whether fqdn falls under one of the declared domains.
func inManagedDomain(cfg *config.Config, fqdn string) bool {
	for domain := range cfg.Domains {
		if strings.HasSuffix(fqdn, "."+strings.ToLower(domain)) {
			return true
		}
	}
	return false
}

// suggestName proposes a service name from a hostname's first label, which is
// the convention across this fleet (status.example.com -> status).
func suggestName(fqdn string) string {
	label, _, _ := strings.Cut(fqdn, ".")
	return label
}
