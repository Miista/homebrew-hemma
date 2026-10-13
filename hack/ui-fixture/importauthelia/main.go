// importauthelia builds the fixture's Authelia configuration.yml from a REAL one — without copying anything secret.
//
// hemma reads exactly three things from that file: each OIDC client's client_id, redirect_uris and
// authorization_policy (internal/auth.autheliaConfigDoc). So this is an ALLOWLIST: it emits only those, and
// everything else in the source (session/JWT/storage keys, client secret hashes, signing keys, SMTP
// credentials, users, ...) is never read into the output. It also neutralizes what it keeps:
//
//   - client_id is replaced by the service's first host label (hemma does not read the real one);
//   - redirect URIs are moved from the real domains to -domain; one under any OTHER host is dropped;
//   - then the output is scanned, and the tool refuses (writing nothing) if a real domain, a secret-looking
//     value or a key block survived.
//
// The source is read from stdin (so the raw file never needs to touch disk): ssh host 'cat …/configuration.yml' | importauthelia …
package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"hemma/internal/config"
)

type srcDoc struct {
	IdentityProviders struct {
		OIDC struct {
			Clients []struct {
				RedirectURIs        []string `yaml:"redirect_uris"`
				AuthorizationPolicy string   `yaml:"authorization_policy"`
			} `yaml:"clients"`
		} `yaml:"oidc"`
	} `yaml:"identity_providers"`
}

type outClient struct {
	ClientID            string   `yaml:"client_id"`
	AuthorizationPolicy string   `yaml:"authorization_policy,omitempty"`
	RedirectURIs        []string `yaml:"redirect_uris"`
}

func main() {
	services := flag.String("services", "", "the SOURCE services.yaml: its declared domains are the real domains to move")
	dst := flag.String("dst", "", "where to write the neutralized configuration.yml")
	domain := flag.String("domain", "example.com", "the domain every redirect URI moves under")
	flag.Parse()
	if *services == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: … | importauthelia -services <source services.yaml> -dst <fixture configuration.yml>")
		os.Exit(2)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	out, summary, err := convert(raw, *services, *domain)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(dirOf(*dst), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*dst, out, 0o644); err != nil {
		fail(err)
	}
	fmt.Println(summary)
}

func fail(err error) { fmt.Fprintln(os.Stderr, "importauthelia:", err); os.Exit(1) }

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func convert(raw []byte, servicesPath, domain string) ([]byte, string, error) {
	sdata, err := os.ReadFile(servicesPath)
	if err != nil {
		return nil, "", err
	}
	sc, err := config.Parse(sdata, servicesPath)
	if err != nil {
		return nil, "", fmt.Errorf("the source services.yaml does not load: %w", err)
	}
	var realDomains []string
	for d := range sc.Domains {
		realDomains = append(realDomains, d)
	}

	var in srcDoc
	if err := yaml.Unmarshal(raw, &in); err != nil {
		return nil, "", fmt.Errorf("the source configuration.yml does not parse: %w", err)
	}

	var clients []outClient
	dropped := 0
	seen := map[string]int{}
	for _, c := range in.IdentityProviders.OIDC.Clients {
		var uris []string
		label := ""
		for _, u := range c.RedirectURIs {
			moved, host, ok := moveURI(u, realDomains, domain)
			if !ok {
				dropped++
				continue
			}
			uris = append(uris, moved)
			if label == "" {
				label = strings.SplitN(host, ".", 2)[0]
			}
		}
		if len(uris) == 0 {
			continue
		}
		seen[label]++
		id := label
		if seen[label] > 1 {
			id = fmt.Sprintf("%s-%d", label, seen[label])
		}
		clients = append(clients, outClient{ClientID: id, AuthorizationPolicy: c.AuthorizationPolicy, RedirectURIs: uris})
	}

	var doc struct {
		IdentityProviders struct {
			OIDC struct {
				Clients []outClient `yaml:"clients"`
			} `yaml:"oidc"`
		} `yaml:"identity_providers"`
	}
	doc.IdentityProviders.OIDC.Clients = clients
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, "", err
	}
	out := "# Fixture only: the NON-SECRET shape of the real OIDC clients (client_id, redirect_uris, authorization_policy),\n" +
		"# which is all hemma reads. Domains are moved to the fixture's; nothing else from the real file is here.\n" + string(body)

	// The safety net: refuse if anything real or secret-looking survived.
	for _, d := range realDomains {
		if strings.Contains(out, d) {
			return nil, "", fmt.Errorf("refusing: the real domain %q survived into the output", d)
		}
	}
	for _, bad := range []string{"secret", "$pbkdf2", "$argon2", "$scrypt", "$bcrypt", "BEGIN", "password", "token", "private_key"} {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(bad)) { // the data, not the explanatory header above it
			return nil, "", fmt.Errorf("refusing: %q appears in the output", bad)
		}
	}
	return []byte(out), fmt.Sprintf("wrote %d OIDC clients (%d redirect URIs dropped for not being under a declared domain)", len(clients), dropped), nil
}

// moveURI rewrites a redirect URI's host from a real domain to the fixture's; ok=false when its host is under none.
func moveURI(raw string, realDomains []string, target string) (moved, host string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	for _, d := range realDomains {
		if u.Host == d || strings.HasSuffix(u.Host, "."+d) {
			u.Host = strings.TrimSuffix(u.Host, d) + target
			return u.String(), u.Host, true
		}
	}
	return "", "", false
}
