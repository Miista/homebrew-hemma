package cli

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"hemma/internal/config"
)

// Read-only detection of which services are reachable from the internet, for
// the EXPOSURE column of `list`.
//
// hemma owns the INTERNAL horizon only (Pi-hole record + Caddy block) and
// still never writes docker-compose.yml (design §12). But "is this FQDN also
// public?" is not answerable from services.yaml at all, and the answer lives
// one file away: the tunnel tool declares public ingress with a compose label
// (defaults.public_label, default cloudflare.io/hostname) whose value is the
// public hostname. So this reads — never writes — each host's compose file
// from the checkout and matches label values against service FQDNs.
//
// Two properties make label presence a trustworthy answer rather than a guess:
// the tunnel container upserts the public DNS record for every hostname it is
// given, so a label implies the public name resolves; and the compose file in
// the checkout is the source the running containers were started from.
//
// The label KEY is configurable so hemma stays generator-agnostic — its core
// generates Pi-hole and Caddy config and knows nothing about any particular
// tunnel. Set defaults.public_label to "none" to switch the column off.
//
// Matching is by FQDN, not by container name: the label may sit on any
// container in the host's compose (a service often fronts through Caddy), and
// the FQDN is the one identifier both sides agree on.

// composeFile is the conventional compose filename in a host's repo directory,
// the same <hostDir>/docker-compose.yml the auth wiring check reads.
const composeFile = "docker-compose.yml"

// Exposure column values.
const (
	exposurePublic  = "public"
	exposureLocal   = "local"
	exposureUnknown = "?"
)

// composeLabelsDoc is the sliver of docker-compose.yml this read needs: the
// labels of each service. Labels stay a yaml.Node because compose allows both
// the map form (key: value) and the list form (- key=value).
type composeLabelsDoc struct {
	Services map[string]struct {
		Labels yaml.Node `yaml:"labels"`
	} `yaml:"services"`
}

// exposureLookup answers "is this FQDN publicly reachable?" per service,
// caching one compose parse per host directory. The zero-value label ("")
// means the feature is off and every answer is "".
type exposureLookup struct {
	label    string
	repoRoot string
	// public maps a host name to the set of lowercased FQDNs labelled public
	// in that host's compose file. A nil set means the file was missing or
	// unparseable — reported as unknown rather than silently as local.
	public map[string]map[string]bool
}

// newExposureLookup prepares the lookup. It reads nothing yet; compose files
// are parsed lazily, one per host, on first service from that host.
func newExposureLookup(repoRoot string, cfg *config.Config) *exposureLookup {
	return &exposureLookup{
		label:    cfg.Defaults.ResolvedPublicLabel(),
		repoRoot: repoRoot,
		public:   map[string]map[string]bool{},
	}
}

// enabled reports whether the EXPOSURE column should be shown at all.
func (e *exposureLookup) enabled() bool { return e != nil && e.label != "" }

// of returns the exposure of one service: "public" when the host's compose
// declares the public-ingress label for its FQDN, "local" when it does not,
// and "?" when that host's compose could not be read. Returns "" when the
// feature is off.
func (e *exposureLookup) of(cfg *config.Config, svc config.Service) string {
	if !e.enabled() {
		return ""
	}
	set, ok := e.public[svc.Host]
	if !ok {
		host := cfg.Hosts[svc.Host]
		dir := host.ResolvedDir(svc.Host)
		set = labelledHostnames(filepath.Join(e.repoRoot, dir, composeFile), e.label)
		e.public[svc.Host] = set
	}
	if set == nil {
		return exposureUnknown
	}
	if set[strings.ToLower(svc.FQDN)] {
		return exposurePublic
	}
	return exposureLocal
}

// labelledHostnames parses the compose file at path and returns the set of
// lowercased hostnames declared by the label key on any of its services. A
// missing or unparseable file returns nil (unknown), which is deliberately
// distinct from an empty set (parsed fine, nothing is public).
func labelledHostnames(path, label string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc composeLabelsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, svc := range doc.Services {
		for _, v := range labelValues(svc.Labels, label) {
			if h := hostnameFromLabel(v); h != "" {
				out[h] = true
			}
		}
	}
	return out
}

// labelValues extracts every value of key from a compose labels node,
// accepting both the map form (key: value) and the list form (- key=value).
func labelValues(labels yaml.Node, key string) []string {
	var out []string
	switch labels.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(labels.Content); i += 2 {
			if labels.Content[i].Value == key {
				out = append(out, labels.Content[i+1].Value)
			}
		}
	case yaml.SequenceNode:
		for _, item := range labels.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok && k == key {
				out = append(out, v)
			}
		}
	}
	return out
}

// hostnameFromLabel normalizes a label value to a bare lowercased hostname.
// The value may carry a backend-port suffix (host.example.com:8123) which is
// the tunnel's business, not ours.
func hostnameFromLabel(v string) string {
	h := strings.TrimSpace(v)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSpace(h))
}
