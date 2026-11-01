package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
)

// adminConfigURL is where the proxy's admin API hands out the config it is RUNNING, as read from inside its container.
// 127.0.0.1, not localhost: the image's busybox wget tries ::1 first and gets refused.
const adminConfigURL = "http://127.0.0.1:2019/config/"

// caddyfileInContainer is the Caddyfile as the container sees it (the host's caddy/data mounted at /etc/caddy, as apply uses).
const caddyfileInContainer = "/etc/caddy/Caddyfile"

// liveTarget is one service on this host whose generated site depends on a file hemma only copies through: its
// extra_directives file, and/or the shared auth snippet it imports.
type liveTarget struct {
	name, fqdn string
	extra      string // the service's extra_directives file, if it has one
	auth       bool   // its site imports (auth), so it carries the auth snippet
}

// liveTargets lists the enabled services on self that carry an extra_directives file or the auth snippet, in name order.
// A service with neither is not this check's business: nothing in it comes from a copied-through file.
func liveTargets(cfg *config.Config, self string) []liveTarget {
	var out []liveTarget
	localNets := len(cfg.Defaults.LocalNetworks) > 0
	for _, name := range sortedKeysOf(cfg.Services) {
		s := cfg.Services[name]
		if s.Host != self || s.Disabled {
			continue
		}
		t := liveTarget{name: name, fqdn: s.FQDN, extra: s.ExtraDirectives}
		t.auth = cfg.Defaults.AuthSnippet != "" && s.GateScope(localNets) != config.GateNone
		if t.extra != "" || t.auth {
			out = append(out, t)
		}
	}
	return out
}

// siteState is how one service's site stands between what is on disk and what is deployed (running).
type siteState int

const (
	siteDeployed  siteState = iota // the same on disk and in the running proxy
	siteChanged                    // on disk, and the running proxy has something else or nothing
	siteNotOnDisk                  // nothing on disk to compare: reported by the drift and import checks, not here
)

// stateOf compares the routes for fqdn in the adapted on-disk config and in the running one. A site missing from the disk side
// is its own state even when the proxy lacks it too: two empty answers are equal, but nothing was compared.
func stateOf(adapted, running any, fqdn string) siteState {
	disk, live := routesForHost(adapted, fqdn), routesForHost(running, fqdn)
	switch {
	case len(disk) == 0:
		return siteNotOnDisk
	case equalStrings(disk, live):
		return siteDeployed
	}
	return siteChanged
}

// diagDeployed answers one question for the two kinds of file hemma only copies through into a site — the auth snippet, and a
// service's extra directives: has what changed on disk been deployed? It asks the running proxy on THIS host what it is
// serving and compares that with what is on disk. It reports on each, deployed or not:
//
//   - one advisory for the auth snippet (deployed.auth:<host>) — the snippet is shared, so it either is deployed or it isn't;
//   - one advisory per service that has extra directives (deployed.extra.<service>:<host>);
//   - and, when the proxy cannot be asked at all, one soft finding for that (deployed.unreachable:<host>).
//
// They are advisories: none affects the exit code, and doctor never deploys anything. What a user needs to know is only that
// something changed on disk and has not been deployed, and that `hemma apply` (on the host) or `hemma deploy` fixes it; why
// the proxy is behind is not for the advisory to explain.
//
// The proxy reads its config when it starts or is told to reload; it does not watch files. So an edited auth snippet or
// extra-directives file reaches the generated site only through `hemma doctor --fix`, and goes live only through apply or
// deploy. "Disk" is the on-disk Caddyfile adapted to JSON by the container's own `caddy adapt`; "deployed" is its admin API's
// /config/. They are compared route by route for each service's host (keys sorted, so the order the proxy happens to marshal in
// does not matter).
//
// The auth snippet is judged by the gated services that carry NO extra directives of their own, since their route depends on
// the snippet alone; judging it by a service whose extras changed would blame the wrong file. If every gated service has extras
// there is no clean sample and nothing is said about the snippet. A service with both features whose route differs is reported
// for its extras only when the snippet is deployed: while the snippet is not, the one deploy that fixes the snippet fixes it too.
//
// A service whose site is not on disk at all is skipped: the drift check (missing generated file) and the Caddyfile-import check
// already report that, and there is nothing to compare.
//
// It runs only where it can mean something: on a host that serves sites, and only when a service there has one of the two
// features (nothing to compare means nothing is asked). Hosts are checked by their own doctor (the UI's fleet view runs it on
// each), since the admin API is reachable from that host only.
func diagDeployed(env *Env, repoRoot string, cfg *config.Config) []Finding {
	self := localHost(cfg)
	if self == "" || !hostRunsCaddy(cfg, self) {
		return nil
	}
	targets := liveTargets(cfg, self)
	if len(targets) == 0 {
		return nil
	}
	out := func(args ...string) ([]byte, error) {
		return env.command("docker", append([]string{"exec", caddyContainer}, args...)...).Output()
	}
	skip := func(why error) []Finding {
		return []Finding{deployedAdvisory(repoRoot, self, deployedUnreachable, auth.Advisory{
			Headline: fmt.Sprintf("could not check whether changes on disk are deployed (%v)", why),
			Body:     []string{"The proxy container must be up, with its admin API answering on 127.0.0.1:2019."},
			Fix:      []string{"start the proxy container and check that its admin API answers on 127.0.0.1:2019"},
		})}
	}
	adaptedRaw, err := out("caddy", "adapt", "--config", caddyfileInContainer, "--adapter", "caddyfile")
	if err != nil {
		return skip(fmt.Errorf("reading the on-disk config: %w", err))
	}
	runningRaw, err := out("curl", "-s", "-m", "5", "-f", adminConfigURL)
	if err != nil {
		return skip(fmt.Errorf("reading the running config: %w", err))
	}
	adapted, err := decodeJSON(adaptedRaw)
	if err != nil {
		return skip(fmt.Errorf("on-disk config: %w", err))
	}
	running, err := decodeJSON(runningRaw)
	if err != nil {
		return skip(fmt.Errorf("running config: %w", err))
	}

	state := map[string]siteState{}
	for _, t := range targets {
		state[t.name] = stateOf(adapted, running, t.fqdn)
	}
	fix := fmt.Sprintf("hemma apply on %s, or hemma deploy", self)

	var fs []Finding

	// The auth snippet: shared, so binary. Judged by the gated services that have no extras of their own.
	snippetKnown, snippetOut := false, false
	for _, t := range targets {
		if t.auth && t.extra == "" && state[t.name] != siteNotOnDisk {
			snippetKnown = true
			snippetOut = snippetOut || state[t.name] != siteDeployed
		}
	}
	if snippetKnown {
		if snippetOut {
			fs = append(fs, deployedAdvisory(repoRoot, self, deployedAuth, auth.Advisory{
				Headline: "the auth snippet changed on disk and not deployed",
				Body:     []string{"source: " + cfg.Defaults.AuthSnippet},
				Fix:      []string{fix},
			}))
		} else {
			fs = append(fs, deployedOK(self, deployedAuth, "the auth snippet is deployed"))
		}
	}

	// Extra directives: one advisory per service that has them.
	for _, t := range targets {
		if t.extra == "" || state[t.name] == siteNotOnDisk {
			continue
		}
		kind := "extra." + t.name
		switch {
		case state[t.name] == siteDeployed:
			fs = append(fs, deployedOK(self, kind, fmt.Sprintf("extra directives for %s are deployed", t.name)))
		case t.auth && snippetKnown && snippetOut:
			// Behind because of the snippet too: the deploy that fixes the snippet fixes this site, and the snippet's own advisory says so.
		default:
			fs = append(fs, deployedAdvisory(repoRoot, self, kind, auth.Advisory{
				Headline: fmt.Sprintf("extra directives for %s changed on disk and not deployed", t.name),
				Body:     []string{"source: " + t.extra},
				Fix:      []string{fix},
			}))
		}
	}
	return fs
}

// The advisories of the deployed.* family, by the kind in their ID.
const (
	deployedAuth        = "auth"        // the shared auth snippet
	deployedUnreachable = "unreachable" // the proxy could not be asked at all
)

// deployedID: the shared snippet is "deployed.auth:<host>:snippet", a service's extra directives are
// "deployed.extra:<host>:<service>" (kind "extra.<service>"), and the proxy that could not be asked is
// "deployed.unreachable:<host>:proxy".
func deployedID(host, kind string) string {
	switch {
	case kind == deployedAuth:
		return findingID("deployed.auth", host, "snippet")
	case kind == deployedUnreachable:
		return findingID("deployed.unreachable", host, "proxy")
	case strings.HasPrefix(kind, "extra."):
		return findingID("deployed.extra", host, strings.TrimPrefix(kind, "extra."))
	}
	return findingID("deployed."+kind, host, kind)
}

// deployedOK is a check that passed: a ✓ line in the CLI, as "Auth config is consistent." is. The Issues page keeps only warnings
// and errors, so it never shows these.
func deployedOK(host, kind, title string) Finding {
	return Finding{ID: deployedID(host, kind), Status: "ok", Title: title}
}

// deployedAdvisory is one problem of the family, built the way every other advisory is: an auth.Advisory (headline, body, fix,
// then) turned into a finding by advisoryFinding, so the CLI prints it in the same grammar (blank line before, dim body, the
// fix: line, the closing note) and a UI gets the same structure. It never counts toward the exit code.
func deployedAdvisory(repoRoot, host, kind string, a auth.Advisory) Finding {
	f := advisoryFinding(repoRoot, a)
	f.ID = deployedID(host, kind)
	return f
}

// decodeJSON parses a JSON document keeping numbers exact (no float round trip), so two equal configs stay equal.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// routesForHost returns, sorted, the canonical JSON of every route in a proxy config whose host matcher names fqdn. Marshalling
// a map writes its keys in sorted order, which is what makes two renderings of one config comparable.
func routesForHost(doc any, fqdn string) []string {
	var found []string
	root, _ := doc.(map[string]any)
	apps, _ := root["apps"].(map[string]any)
	httpApp, _ := apps["http"].(map[string]any)
	servers, _ := httpApp["servers"].(map[string]any)
	for _, sv := range servers {
		srv, _ := sv.(map[string]any)
		routes, _ := srv["routes"].([]any)
		for _, r := range routes {
			if routeNamesHost(r, fqdn) {
				if b, err := json.Marshal(r); err == nil {
					found = append(found, string(b))
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// routeNamesHost reports whether one route's matchers include a host equal to fqdn.
func routeNamesHost(route any, fqdn string) bool {
	rm, _ := route.(map[string]any)
	matchers, _ := rm["match"].([]any)
	for _, m := range matchers {
		mm, _ := m.(map[string]any)
		hosts, _ := mm["host"].([]any)
		for _, h := range hosts {
			if s, _ := h.(string); strings.EqualFold(s, fqdn) {
				return true
			}
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
