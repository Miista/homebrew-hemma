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
// Every check here is scoped to a service DECLARED in services.yaml — hemma
// reports on what it manages, never on hostnames outside its source of truth.
// (There was once an "orphan ingress" check that flagged publicly-served
// hostnames with no service entry; it was removed because it reported on
// something hemma neither manages nor could generate, and its suggested fix —
// `hemma add service …` — could not always be fulfilled. A hostname absent from
// services.yaml is out of scope by definition.)
//
// Three checks, in descending severity, all counting as doctor problems:
//
//  1. AUTH BYPASS — a forward-auth service whose tunnel ingress points DIRECT
//     at the container. The tunnel never traverses Caddy, so the (auth) snippet
//     never runs and the service is publicly reachable with no authentication
//     at all. This is the only check here that reports a live security hole.
//  2. DECLARED BUT NOT SERVED — the service says `public: true` and no tunnel
//     label backs that up: hemma wired the internal half, the public half was
//     never done.
//  3. UNDECLARED EXPOSURE — the tunnel serves a declared service that never
//     opted in. Absent `public` means local-only, so this is exposure nobody
//     wrote down — the check that catches an ACCIDENTAL label. Fires in bulk
//     exactly once, when a repo first declares its existing public surface.

// publicHorizonWarnings runs the four public-horizon checks. Silent when
// public-horizon reporting is off, and per-host silent when that host's compose
// file cannot be read (an unreadable file is not evidence of anything).
func publicHorizonWarnings(repoRoot string, cfg *config.Config) (advs []auth.Advisory, problems int) {
	pub := newPublicLookup(repoRoot, cfg)
	if !pub.enabled() {
		return nil, 0
	}
	var undeclared []exposedService

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

		// --- 2. opted in but not served ---
		if a, hit := declaredPublicAdvisory(cfg, name, svc, served, composePath, pub); hit {
			advs = append(advs, a)
			problems++
		}

		// --- 3. served but never opted in ---
		if served && !svc.Public {
			undeclared = append(undeclared, exposedService{name: name, fqdn: svc.FQDN, compose: composePath})
		}
	}
	if a, hit := undeclaredExposureAdvisory(undeclared, pub); hit {
		advs = append(advs, a)
		problems++
	}
	return advs, problems
}

// exposedService is one service reachable from the internet without having
// opted in.
type exposedService struct{ name, fqdn, compose string }

// undeclaredExposureAdvisory reports services the tunnel serves that never
// declared `public: true`. Under "assume local, opt in to public", absent IS the
// statement that a service should be LAN-only — so a label on one of them means
// it became publicly reachable without that being written down anywhere. This is
// the check that catches ACCIDENTAL exposure, which is the failure that actually
// costs something (an admin UI reachable from the internet).
//
// Grouped into ONE advisory rather than one per service, because it fires in
// bulk exactly once — when a repo first adopts the field and has to declare its
// existing public surface. After that it is normally a single entry.
//
// Deliberately NOT --fix-able even though the fix (adding `public: true`) is to
// services.yaml, a file hemma owns and could safely write. Auto-adopting
// observed exposure into the declaration would silence this alarm by definition:
// the one case it exists for — a label nobody meant to add — would be rewritten
// into "intended" without a human ever seeing it. So the advisory presents BOTH
// resolutions, and removing the label comes first: if the exposure is a mistake,
// declaring it is the wrong repair.
func undeclaredExposureAdvisory(exposed []exposedService, pub *publicLookup) (auth.Advisory, bool) {
	if len(exposed) == 0 {
		return auth.Advisory{}, false
	}
	sort.Slice(exposed, func(i, j int) bool { return exposed[i].name < exposed[j].name })

	verb := "is"
	if len(exposed) > 1 {
		verb = "are"
	}
	body := []string{
		fmt.Sprintf("each of these carries a %s label, making it reachable from the", pub.label),
		"internet, but nothing in services.yaml says that was intended:",
	}
	for _, e := range exposed {
		body = append(body, fmt.Sprintf("  %-14s %s", e.name, e.fqdn))
	}
	body = append(body,
		"No `public: true` means local-only, so as far as hemma is concerned each of",
		"these is exposed by accident until you say otherwise.")

	fix := []string{"if the exposure is NOT intended, remove the label from that host's compose file;"}
	fix = append(fix, "if it IS intended, record it so this stops being a finding:")
	for _, e := range exposed {
		fix = append(fix, fmt.Sprintf("  hemma update service %s --public", e.name))
	}
	return auth.Advisory{
		Headline: fmt.Sprintf("%d %s %s publicly exposed without `public: true`",
			len(exposed), plural(len(exposed), "service"), verb),
		Body: body,
		Fix:  fix,
	}, true
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

// declaredPublicAdvisory reports a service that opted into the public horizon
// (`public: true`) but has no tunnel label to back it up — the §12 gotcha, made
// visible: hemma did its job internally while the public half was never wired.
//
// This is the "wanted public, did not get it" direction; the reverse — served
// without an opt-in — is undeclaredExposureAdvisory. Locality is never part of
// either comparison: every service is reachable on the LAN regardless, directly
// via Pi-hole or by hairpin.
func declaredPublicAdvisory(cfg *config.Config, name string, svc config.Service, served bool, composePath string, pub *publicLookup) (auth.Advisory, bool) {
	if !svc.Public || served {
		return auth.Advisory{}, false
	}
	// Both resolutions, for the same reason the reverse check offers both: if
	// the opt-in was the mistake, adding a label is the wrong repair — and this
	// is the direction where following the advice blindly puts a service on the
	// internet, so the alternative must be visible rather than inferred.
	fix := publicLabelSnippet(cfg, name, svc, pub)
	fix = append(fix,
		"or, if it is not meant to be public after all, opt back out:",
		fmt.Sprintf("  hemma update service %s --public=false", name))
	return auth.Advisory{
		Headline: fmt.Sprintf("%s is declared public but has no public ingress", name),
		Body: []string{
			fmt.Sprintf("no %s label in %s names %s,", pub.label, composePath, svc.FQDN),
			"so the tunnel does not serve it and the name has no public DNS record.",
			"On the LAN it resolves (hemma generated that); from the internet it does not.",
		},
		Fix:  fix,
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
