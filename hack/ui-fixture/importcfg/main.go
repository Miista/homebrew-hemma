// importcfg turns a REAL services.yaml into one the synthetic fixture can run: the same services, names, auth
// and tunnel settings, with every host, domain and address replaced so that nothing real can be reached or
// leaked from the fixture.
//
//   - every domain collapses into -domain (the fixture has a throwaway certificate for exactly that one);
//   - every host collapses into -host at -host-ip: the fixture is one machine, and a second declared host would
//     make `deploy` try to SSH to it;
//   - `ssh:` destinations are dropped, and trusted proxies become the fixture's own cloudflared stub.
//
// It then scans its own output and refuses if anything from the source's hosts, domains, addresses or ssh
// destinations survived. The source is only read.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"hemma/internal/config"
)

func main() {
	src := flag.String("src", "", "the real services.yaml (read only)")
	dst := flag.String("dst", "", "where to write the neutralized services.yaml")
	domain := flag.String("domain", "example.com", "the one domain every service moves under")
	host := flag.String("host", "edge", "the one fixture host every service moves to")
	hostIP := flag.String("host-ip", "172.28.0.10", "the fixture host's address")
	proxy := flag.String("proxy", "172.28.0.13/32", "the fixture's trusted proxy (its cloudflared stub)")
	flag.Parse()
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: importcfg -src <real services.yaml> -dst <fixture services.yaml>")
		os.Exit(2)
	}
	if err := run(*src, *dst, *domain, *host, *hostIP, *proxy); err != nil {
		fmt.Fprintln(os.Stderr, "importcfg:", err)
		os.Exit(1)
	}
}

func run(src, dst, domain, host, hostIP, proxy string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	in, err := config.Parse(data, src)
	if err != nil {
		return fmt.Errorf("the source does not load: %w", err)
	}

	// What must NOT survive: the source's real domains, addresses and ssh destinations (and host names other than ours).
	var forbid []string
	srcDomains := make([]string, 0, len(in.Domains))
	for d := range in.Domains {
		srcDomains = append(srcDomains, d)
		forbid = append(forbid, d)
	}
	sort.Strings(srcDomains)
	for name, h := range in.Hosts {
		forbid = append(forbid, h.IP, h.SSH)
		if name != host {
			forbid = append(forbid, name)
		}
		for _, p := range h.TrustedProxies {
			forbid = append(forbid, p)
		}
	}

	out, err := config.Parse(data, dst)
	if err != nil {
		return err
	}
	out.Domains = map[string]config.Domain{domain: {}}
	out.Hosts = map[string]config.Host{host: {IP: hostIP, TrustedProxies: []string{proxy}}}
	out.Defaults.DNSHost = host
	if out.Defaults.DeployHost != "" {
		out.Defaults.DeployHost = host
	}
	seen := map[string]string{}
	for name, s := range out.Services {
		fq, err := moveDomain(s.FQDN, srcDomains, domain)
		if err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if other, dup := seen[fq]; dup {
			return fmt.Errorf("services %q and %q would both be %s after the domains collapse", other, name, fq)
		}
		seen[fq] = name
		s.FQDN, s.Host = fq, host
		out.Services[name] = s
	}
	if err := out.Save(); err != nil {
		return err
	}

	// The safety net: read back what was written.
	written, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	for _, f := range forbid {
		if f != "" && strings.Contains(string(written), f) {
			os.Remove(dst)
			return fmt.Errorf("refusing: %q from the source survived into the output", f)
		}
	}
	if _, err := config.Parse(written, dst); err != nil {
		os.Remove(dst)
		return fmt.Errorf("the output does not load: %w", err)
	}
	disabled, tunnel := 0, 0
	for _, s := range out.Services {
		if s.Disabled {
			disabled++
		}
		if s.Tunnel {
			tunnel++
		}
	}
	fmt.Printf("imported %d services (%d disabled, %d with a tunnel) onto host %s under %s\n", len(out.Services), disabled, tunnel, host, domain)
	return nil
}

func moveDomain(fqdn string, srcDomains []string, target string) (string, error) {
	for _, d := range srcDomains {
		if fqdn == d {
			return target, nil
		}
		if strings.HasSuffix(fqdn, "."+d) {
			return strings.TrimSuffix(fqdn, d) + target, nil
		}
	}
	return "", fmt.Errorf("%q is not under any declared domain", fqdn)
}
