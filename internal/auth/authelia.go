package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"hemma/internal/render"
)

// authelia is the Provider implementation for Authelia
// (https://www.authelia.com). It owns the generated
// hemma.access_control.generated.yml artifact and the read-only validation
// of configuration.yml (OIDC clients).
type authelia struct{}

func init() { Register(authelia{}) }

const (
	// autheliaConfigPath is the fixed convention path (relative to the
	// auth_service host's repo directory) of the Authelia configuration file
	// that declares OIDC clients. Read-only; hemma never writes it.
	autheliaConfigPath = "authelia/data/config/configuration.yml"
	// autheliaAccessControlFile is the generated access-control artifact,
	// written next to configuration.yml (same config dir). Authelia must be
	// told to include it (it is not auto-loaded); hemma only generates it.
	autheliaAccessControlFile = "hemma.access_control.generated.yml"
)

func (authelia) Name() string       { return DefaultName }
func (authelia) ConfigPath() string { return autheliaConfigPath }

// ApplyCommands: Authelia has no hot reload — validate in-container (this
// respects X_AUTHELIA_CONFIG multi-file setups), then restart the container.
// Sessions survive when they live in an external store (e.g. redis).
func (authelia) ApplyCommands(container string) (validate, reload []string) {
	return []string{"docker", "exec", container, "authelia", "config", "validate"},
		[]string{"docker", "restart", container}
}

// AccessControl renders access_control rules for forward-auth services and
// oidc authorization_policies for oidc services that declare groups.
//
// Authelia subject semantics (important, and easy to get backwards): a
// subject item that is a plain string or a flat list of strings is an AND of
// its criteria; OR requires a list of LISTS (each inner list is one AND
// clause, outer items are OR'd). Membership in ANY of a service's groups must
// grant access, so multiple groups are emitted as nested single-element
// lists.
func (authelia) AccessControl(services []Service) (path, content string, ok bool) {
	var forward, oidcWithGroups []Service
	for _, s := range services {
		switch {
		case s.Mode == ModeForward:
			forward = append(forward, s)
		case s.Mode == ModeOIDC && len(s.Groups) > 0:
			oidcWithGroups = append(oidcWithGroups, s)
		}
	}
	if len(forward) == 0 && len(oidcWithGroups) == 0 {
		return "", "", false
	}
	// Stable output: alphabetical by service name.
	sort.Slice(forward, func(i, j int) bool { return forward[i].Name < forward[j].Name })
	sort.Slice(oidcWithGroups, func(i, j int) bool { return oidcWithGroups[i].Name < oidcWithGroups[j].Name })

	var b strings.Builder
	b.WriteString(render.Header + "\n")

	if len(forward) > 0 {
		b.WriteString("access_control:\n")
		b.WriteString("  default_policy: 'deny'\n")
		b.WriteString("  rules:\n")
		for _, s := range forward {
			// Bypass rules first (one per public path), then the access rule —
			// Authelia rules are first-match, so the exemptions must precede
			// the gate, mirroring the Caddy handle-block ordering (§4.5).
			for _, p := range s.PublicPaths {
				fmt.Fprintf(&b, "    - domain: %s\n", yq(s.FQDN))
				b.WriteString("      resources:\n")
				fmt.Fprintf(&b, "        - %s\n", yq(pathResource(p)))
				b.WriteString("      policy: 'bypass'\n")
			}
			fmt.Fprintf(&b, "    - domain: %s\n", yq(s.FQDN))
			b.WriteString("      policy: 'one_factor'\n")
			writeSubject(&b, "      ", s.Groups)
		}
	}

	if len(oidcWithGroups) > 0 {
		if len(forward) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("identity_providers:\n")
		b.WriteString("  oidc:\n")
		b.WriteString("    authorization_policies:\n")
		for _, s := range oidcWithGroups {
			fmt.Fprintf(&b, "      %s:\n", s.Name)
			b.WriteString("        default_policy: 'deny'\n")
			b.WriteString("        rules:\n")
			b.WriteString("          - policy: 'one_factor'\n")
			writeSubject(&b, "            ", s.Groups)
		}
	}

	dir := filepath.Dir(autheliaConfigPath)
	return filepath.Join(dir, autheliaAccessControlFile), b.String(), true
}

// writeSubject emits a rule's subject at the given indent. Omitted entirely
// when no groups are set (any authenticated user). A single group is a plain
// list item; multiple groups are nested single-element lists so Authelia ORs
// them (see AccessControl doc).
func writeSubject(b *strings.Builder, indent string, groups []string) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(b, "%ssubject:\n", indent)
	if len(groups) == 1 {
		fmt.Fprintf(b, "%s  - %s\n", indent, yq("group:"+groups[0]))
		return
	}
	for _, g := range groups {
		fmt.Fprintf(b, "%s  - [%s]\n", indent, yq("group:"+g))
	}
}

// pathResource translates a public_paths entry into an Authelia resources
// regex, mirroring the Caddy path-matcher semantics render.CaddySite relies
// on: the literal path (regex meta escaped), a trailing /* meaning "and
// anything below", and an optional query string. e.g. /health →
// ^/health([/?].*)?$.
func pathResource(p string) string {
	base := strings.TrimSuffix(p, "/*")
	return "^" + regexp.QuoteMeta(base) + "([/?].*)?$"
}

// yq single-quotes a YAML scalar (Authelia docs style), doubling any embedded
// single quotes.
func yq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// autheliaConfigDoc is the (partial) shape of configuration.yml this provider
// reads for validation.
type autheliaConfigDoc struct {
	IdentityProviders struct {
		OIDC struct {
			Clients []struct {
				ClientID            string   `yaml:"client_id"`
				RedirectURIs        []string `yaml:"redirect_uris"`
				AuthorizationPolicy string   `yaml:"authorization_policy"`
			} `yaml:"clients"`
		} `yaml:"oidc"`
	} `yaml:"identity_providers"`
}

// ValidateConfig verifies, read-only, that each oidc service has an Authelia
// OIDC client registering a redirect_uri on https://<fqdn>/, and — when the
// service declares groups (so a named authorization_policy is generated) —
// that the matching client references that policy. A missing/unparseable
// file is a soft advisory (report-but-proceed), not a hard failure —
// hemma validates OIDC but does not own it. The match is fqdn-only:
// callback paths are app-defined (/login, /oidc/callback/,
// /accounts/oidc/<provider>/...) and unknown to hemma.
func (authelia) ValidateConfig(cfgPath string, services []Service) []string {
	var oidcSvcs []Service
	for _, s := range services {
		if s.Mode == ModeOIDC {
			oidcSvcs = append(oidcSvcs, s)
		}
	}
	if len(oidcSvcs) == 0 {
		return nil
	}
	sort.Slice(oidcSvcs, func(i, j int) bool { return oidcSvcs[i].Name < oidcSvcs[j].Name })

	var w []string
	doc, err := readAutheliaConfig(cfgPath)
	if err != nil {
		for _, s := range oidcSvcs {
			w = append(w, fmt.Sprintf("could not verify OIDC client for %s: %v", s.Name, err))
		}
		return w
	}
	for _, s := range oidcSvcs {
		want := fmt.Sprintf("https://%s/", s.FQDN)
		matchedPolicy := ""
		matched := false
		for _, c := range doc.IdentityProviders.OIDC.Clients {
			for _, uri := range c.RedirectURIs {
				if strings.HasPrefix(uri, want) {
					matched = true
					matchedPolicy = c.AuthorizationPolicy
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			w = append(w, fmt.Sprintf("service %s is auth: oidc but no Authelia OIDC client registers a redirect_uri for %s — register the client in %s ('hemma create app oidc %s' prints a paste-in snippet).", s.Name, want, cfgPath, s.Name))
			continue
		}
		// Groups generate a named authorization_policy (see AccessControl);
		// it only takes effect if the client opts into it by name.
		if len(s.Groups) > 0 && matchedPolicy != s.Name {
			w = append(w, fmt.Sprintf("service %s has auth groups but its Authelia OIDC client does not set authorization_policy: '%s' — set it in %s or the generated group policy never applies.", s.Name, s.Name, cfgPath))
		}
	}
	return w
}

// readAutheliaConfig reads and parses configuration.yml (read-only). A read
// or parse error is returned so callers can emit a soft advisory.
func readAutheliaConfig(path string) (*autheliaConfigDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc autheliaConfigDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return &doc, nil
}
