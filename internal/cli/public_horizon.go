package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"hemma/internal/config"
)

// Read-only detection of which services are reachable from the internet, for
// the PUBLIC column of `list`.
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

// composeOverrideFile is hemma's own generated companion (render.
// ComposeOverrideFilename): Compose auto-merges it over composeFile with zero
// flags, so once a service's cloudflare.io/* labels live only here, reading
// composeFile alone would see nothing for it. Both files are parsed and their
// labels merged label-key-by-label-key, override winning on conflict — the
// same rule Compose itself applies — so this stays correct whether a
// service's labels are hand-written, hemma-generated, or (mid-migration)
// split across both.
const composeOverrideFile = "docker-compose.override.yml"

// PUBLIC column values, matching the vocabulary of the `public` field.
const (
	publicYes     = "yes"
	publicNo      = "no"
	publicUnknown = "?"
)

// composeLabelsDoc is the sliver of a compose file this read needs: each
// service's labels, plus the container_name override so a suggested label
// snippet names the right compose service. Labels stay a yaml.Node because
// compose allows both the map form (key: value) and the list form (- key=value).
type composeLabelsDoc struct {
	Services map[string]composeService `yaml:"services"`
}

// composeService is one service entry's sliver of a compose file, from either
// the base file or its override — the shape mergeComposeLabels combines.
type composeService struct {
	ContainerName string    `yaml:"container_name"`
	Labels        yaml.Node `yaml:"labels"`
}

// ingress is one publicly-served hostname as declared in a compose file.
type ingress struct {
	// Container is the compose service key (or its container_name override)
	// carrying the label — what a suggested snippet must be added under.
	Container string
	// Port is the backend port from the label value's ":port" suffix, empty
	// when the label omits it (the tunnel then infers it from the container).
	Port string
	// Proxied reports whether a proxy label routes this hostname through a
	// reverse proxy instead of straight at the container. Any non-empty value
	// counts: hemma deliberately does not try to verify the target IS the Caddy
	// it generates config for, because guessing wrong would mean crying wolf on
	// a working setup. Absence is the unambiguous case, and the only one acted on.
	Proxied bool
}

// publicLookup answers "is this FQDN publicly reachable?" per service,
// caching one compose parse per host directory. The zero-value label ("")
// means the feature is off and every answer is "".
type publicLookup struct {
	label      string
	proxyLabel string
	repoRoot   string
	// public maps a host name to its lowercased publicly-served hostnames. A
	// nil map means the compose file was missing or unparseable — reported as
	// unknown rather than silently as not-public.
	public map[string]map[string]ingress
}

// newPublicLookup prepares the lookup. It reads nothing yet; compose files
// are parsed lazily, one per host, on first service from that host.
func newPublicLookup(repoRoot string, cfg *config.Config) *publicLookup {
	return &publicLookup{
		label:      cfg.Defaults.ResolvedPublicLabel(),
		proxyLabel: cfg.Defaults.ResolvedPublicProxyLabel(),
		repoRoot:   repoRoot,
		public:     map[string]map[string]ingress{},
	}
}

// enabled reports whether the PUBLIC column should be shown at all.
func (e *publicLookup) enabled() bool { return e != nil && e.label != "" }

// hostIngress returns the publicly-served hostnames declared in one host's
// compose file (merged with its override, if any), parsing at most once per
// lookup. A nil result means the base compose file could not be read — the
// override alone is never treated as authoritative, since a host with no
// compose file at all is not a real host, whereas no override file just means
// no service on that host is (yet) hemma-generated.
func (e *publicLookup) hostIngress(cfg *config.Config, host string) map[string]ingress {
	if set, ok := e.public[host]; ok {
		return set
	}
	dir := filepath.Join(e.repoRoot, cfg.Hosts[host].ResolvedDir(host))
	set := labelledIngress(filepath.Join(dir, composeFile), filepath.Join(dir, composeOverrideFile), e.label, e.proxyLabel)
	e.public[host] = set
	return set
}

// of returns the observed public horizon of one service: "yes" when the host's
// compose declares the public-ingress label for its FQDN, "no" when it does
// not, and "?" when that host's compose could not be read. Returns "" when the
// feature is off.
func (e *publicLookup) of(cfg *config.Config, svc config.Service) string {
	if !e.enabled() {
		return ""
	}
	set := e.hostIngress(cfg, svc.Host)
	if set == nil {
		return publicUnknown
	}
	if _, ok := set[strings.ToLower(svc.FQDN)]; ok {
		return publicYes
	}
	return publicNo
}

// labelledIngress parses the base compose file at basePath, merges in the
// override at overridePath if present, and returns every hostname declared by
// the label key across both, keyed by lowercased hostname. A missing or
// unparseable BASE file returns nil (unknown), which is deliberately distinct
// from an empty map (parsed fine, nothing is public) — a missing override is
// not an error, since most hosts won't have one until they're migrated.
// proxyLabel may be empty, which leaves every Proxied false and so disables
// the auth-bypass check.
func labelledIngress(basePath, overridePath, label, proxyLabel string) map[string]ingress {
	base, err := parseComposeLabels(basePath)
	if err != nil {
		return nil
	}
	// A missing/unparseable override is silently ignored (base-only result) —
	// mirrors Compose itself, which just runs on the base file alone when its
	// optional override is absent.
	override, _ := parseComposeLabels(overridePath)
	merged := mergeComposeLabels(base, override)

	out := map[string]ingress{}
	for name, svc := range merged {
		container := name
		if svc.ContainerName != "" {
			container = svc.ContainerName
		}
		proxied := proxyLabel != "" && len(labelValues(svc.Labels, proxyLabel)) > 0
		for _, v := range labelValues(svc.Labels, label) {
			h, port := hostnameAndPort(v)
			if h == "" {
				continue
			}
			out[h] = ingress{Container: container, Port: port, Proxied: proxied}
		}
	}
	return out
}

// parseComposeLabels reads and unmarshals one compose file's services map.
func parseComposeLabels(path string) (map[string]composeService, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc composeLabelsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Services, nil
}

// mergeComposeLabels combines base and override service maps the way Compose
// itself merges two files: per service, override's container_name wins if
// set, and labels merge label-key-by-label-key with override's value winning
// on conflict (verified against real `docker compose config` behavior — the
// second file's labels are not a wholesale replacement of the first's).
// override may be nil (no override file present).
func mergeComposeLabels(base, override map[string]composeService) map[string]composeService {
	if len(override) == 0 {
		return base
	}
	out := make(map[string]composeService, len(base)+len(override))
	for name, svc := range base {
		out[name] = svc
	}
	for name, ov := range override {
		bs, ok := out[name]
		if !ok {
			out[name] = ov
			continue
		}
		merged := bs
		if ov.ContainerName != "" {
			merged.ContainerName = ov.ContainerName
		}
		merged.Labels = mergeLabelNodes(bs.Labels, ov.Labels)
		out[name] = merged
	}
	return out
}

// mergeLabelNodes merges two compose labels yaml.Nodes key-by-key, override
// winning on a shared key. Only the map form (key: value) is supported for the
// override side, since that is the only form hemma itself ever generates
// (render.ComposeOverride); a base file using the list form is read as-is and
// simply loses to any override key that collides, which the label helpers
// below already treat correctly since they scan by Kind independently.
func mergeLabelNodes(base, override yaml.Node) yaml.Node {
	if override.Kind == 0 {
		return base
	}
	if base.Kind == 0 {
		return override
	}
	baseVals := map[string]string{}
	switch base.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(base.Content); i += 2 {
			baseVals[base.Content[i].Value] = base.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range base.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok {
				baseVals[k] = v
			}
		}
	}
	switch override.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(override.Content); i += 2 {
			baseVals[override.Content[i].Value] = override.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range override.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok {
				baseVals[k] = v
			}
		}
	}
	node := yaml.Node{Kind: yaml.MappingNode}
	keys := make([]string, 0, len(baseVals))
	for k := range baseVals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: baseVals[k]})
	}
	return node
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

// hostnameAndPort splits a label value into its hostname and optional port.
func hostnameAndPort(v string) (host, port string) {
	s := strings.TrimSpace(v)
	if _, p, ok := strings.Cut(s, ":"); ok {
		port = strings.TrimSpace(p)
	}
	return hostnameFromLabel(s), port
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
