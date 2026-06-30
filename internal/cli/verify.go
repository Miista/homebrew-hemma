package cli

import (
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"

	"shd/internal/config"
	"shd/internal/plan"
)

// Container names are the homelab convention.
const (
	piholeContainer = "pihole"
	caddyContainer  = "caddy"
)

// cmdVerify checks that a service actually resolves and is served, live.
//
//	shd verify [<fqdn>]
//
// Verification is split across hosts: the DNS half can only be checked on the
// resolver (the pihole host), the Caddy half only on the host that runs the
// service. cmdVerify identifies which host it is running on by matching local
// IPs against the hosts map, then runs the half it can. Run it on each host to
// cover the whole chain.
func cmdVerify(cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "verify")
	if cfg == nil {
		return code
	}

	// Which managed host are we on? (match a local IP against hosts[].ip)
	self := localHost(cfg)
	if self == "" {
		errf("This machine's IP matches no host in services.yaml — run verify on a managed host (the resolver or a service host).")
		return 1
	}
	fmt.Printf("Running on host %q.\n", self)

	// Which services to check: one fqdn, or all.
	var services []string
	if len(args) > 0 {
		fqdn := args[0]
		name := serviceByFQDN(cfg, fqdn)
		if name == "" {
			errf("No service with fqdn %q in services.yaml.", fqdn)
			return 1
		}
		services = []string{name}
	} else {
		p := plan.Build(cfg)
		for _, k := range p.Valid() {
			if !plan.IsDomainOwner(k) {
				services = append(services, k)
			}
		}
		sort.Strings(services)
	}

	resolver := cfg.DNSHost()
	v := &verifier{}
	for _, name := range services {
		svc := cfg.Services[name]
		hostIP := cfg.Hosts[svc.Host].IP
		fmt.Printf("\n%s (%s)\n", name, svc.FQDN)

		if self == resolver {
			v.dns(svc.FQDN, hostIP)
		}
		if self == svc.Host {
			v.caddy(svc.FQDN, svc.Backend)
		}
		if self != resolver && self != svc.Host {
			fmt.Printf("  · this host is neither the resolver (%s) nor the service host (%s) for %s — nothing to check here\n",
				resolver, svc.Host, name)
		}
	}

	fmt.Println()
	if v.fail > 0 {
		errf("%d %s failed.", v.fail, plural(v.fail, "check"))
		return 1
	}
	fmt.Printf("All checks passed (%d).\n", v.pass)
	return 0
}

type verifier struct{ pass, fail int }

func (v *verifier) ok(f string, a ...any) { fmt.Printf("  ✓ "+f+"\n", a...); v.pass++ }
func (v *verifier) no(f string, a ...any) { fmt.Printf("  ✗ "+f+"\n", a...); v.fail++ }

// dns runs the resolver-side checks (in the pihole container).
func (v *verifier) dns(fqdn, wantIP string) {
	// A record must be answered LOCALLY with the host IP. If the conf isn't
	// loaded, pihole forwards upstream and returns the public IP instead.
	a := dexec(piholeContainer, "dig", "+short", "A", fqdn, "@127.0.0.1")
	if a == wantIP {
		v.ok("DNS A = %s (internal)", a)
	} else {
		v.no("DNS A = %s (want %s) — conf not loaded? pihole forwarding upstream. The .conf must be directly in /etc/dnsmasq.d/ (conf-dir does not recurse into subdirs).", orNone(a), wantIP)
	}

	// Confirm answered locally (dnsmasq 'config'), not forwarded/cached.
	dexec(piholeContainer, "dig", fqdn, "@127.0.0.1")
	log := dexecSh(piholeContainer, "tail -n 30 /var/log/pihole/pihole.log 2>/dev/null")
	switch {
	case strings.Contains(log, "config "+fqdn+" is "+wantIP):
		v.ok("answered locally (dnsmasq 'config')")
	case strings.Contains(log, "forwarded "+fqdn):
		v.no("dnsmasq forwarded %s upstream — local record not in effect", fqdn)
	case strings.Contains(log, "cached "+fqdn):
		v.no("dnsmasq served a cached (stale upstream) answer — restart pihole to flush")
	}

	// AAAA must be :: (suppression), never ::1.
	switch aaaa := dexec(piholeContainer, "dig", "+short", "AAAA", fqdn, "@127.0.0.1"); aaaa {
	case "::":
		v.ok("DNS AAAA = :: (suppressed)")
	case "::1":
		v.no("DNS AAAA = ::1 (loopback) — must be :: (unspecified)")
	default:
		v.no("DNS AAAA = %s (want ::)", orNone(aaaa))
	}

	// HTTPS/SVCB (type 65) must be NODATA, else SVCB-aware clients (Safari) take
	// the public endpoint hint and bypass split-horizon.
	if h := dexec(piholeContainer, "dig", "+short", "-t", "TYPE65", fqdn, "@127.0.0.1"); h == "" {
		v.ok("DNS HTTPS/SVCB = NODATA (no public-endpoint leak)")
	} else {
		v.no("DNS HTTPS/SVCB record present — leaks public endpoint to SVCB clients")
	}
}

// caddy runs the service-host-side checks (in the caddy container).
func (v *verifier) caddy(fqdn, backend string) {
	const cf = "/etc/caddy/Caddyfile"
	// imports present AND tls before sites (snippets must parse before use).
	tlsLn := dexecSh(caddyContainer, "grep -nE 'import[[:space:]]+tls/' "+cf+" 2>/dev/null | head -1 | cut -d: -f1")
	sitesLn := dexecSh(caddyContainer, "grep -nE 'import[[:space:]]+sites/' "+cf+" 2>/dev/null | head -1 | cut -d: -f1")
	switch {
	case tlsLn == "" || sitesLn == "":
		v.no("Caddyfile missing 'import tls/*' and/or 'import sites/*'")
	case atoi(tlsLn) < atoi(sitesLn):
		v.ok("Caddyfile imports tls/ before sites/ (correct order)")
	default:
		v.no("Caddyfile imports sites/ before tls/ — snippets undefined, validate fails. Put 'import tls/*.caddy' first.")
	}

	// In adapted config (the file), and validates.
	if strings.Contains(strings.ToLower(dexecShAll(caddyContainer, "caddy adapt --config "+cf+" --adapter caddyfile 2>/dev/null")), strings.ToLower(fqdn)) {
		v.ok("%s in adapted config", fqdn)
	} else {
		v.no("%s not in adapted config", fqdn)
	}
	if dexecOK(caddyContainer, "caddy validate --config "+cf+" --adapter caddyfile") {
		v.ok("caddy validate passes")
	} else {
		v.no("caddy validate FAILED")
	}

	// In the RUNNING config (admin API) — catches an unreloaded change. The
	// admin endpoint isn't always reachable (bound elsewhere/disabled); when
	// the API returns nothing, skip rather than false-fail — the live-HTTPS
	// check below is the real proof Caddy is serving.
	running := dexecShAll(caddyContainer, "wget -qO- http://localhost:2019/config/ 2>/dev/null")
	switch {
	case running == "":
		fmt.Printf("  · running-config check skipped (admin API unreachable); relying on live response\n")
	case strings.Contains(strings.ToLower(running), strings.ToLower(fqdn)):
		v.ok("%s in running config", fqdn)
	default:
		v.no("%s not in running config — reload: docker exec caddy caddy reload --config %s", fqdn, cf)
	}

	// reverse_proxy backend matches the declared backend.
	site := dexecShAll(caddyContainer, "cat /etc/caddy/sites/*.caddy 2>/dev/null")
	if strings.Contains(site, backend) {
		v.ok("reverse_proxy uses %s", backend)
	} else {
		v.no("reverse_proxy does not use %s (declared backend)", backend)
	}

	// Live HTTPS from local Caddy (fresh connection).
	code := dexecSh(caddyContainer, fmt.Sprintf("curl -sk -o /dev/null -w '%%{http_code}' --resolve %s:443:127.0.0.1 https://%s/ 2>/dev/null", fqdn, fqdn))
	if code != "" && code != "000" {
		v.ok("local Caddy answered HTTPS (%s)", code)
	} else {
		v.no("no HTTPS response from local Caddy (%s)", orNone(code))
	}
}

// localHost returns the name of the host in cfg whose IP is assigned to a
// local network interface, or "" if none matches.
func localHost(cfg *config.Config) string {
	local := map[string]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
			}
		}
	}
	for name, h := range cfg.Hosts {
		if local[h.IP] {
			return name
		}
	}
	return ""
}

func serviceByFQDN(cfg *config.Config, fqdn string) string {
	for name, s := range cfg.Services {
		if s.FQDN == fqdn {
			return name
		}
	}
	return ""
}

// dexec runs `docker exec <container> <args...>` and returns the first
// non-empty output line, trimmed ("" on error).
func dexec(container string, args ...string) string {
	out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

// dexecSh runs a shell command inside the container, returns the first
// non-empty line (for single-value output like a line number or http_code).
func dexecSh(container, sh string) string {
	out, err := exec.Command("docker", "exec", container, "sh", "-c", sh).Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

// dexecShAll runs a shell command inside the container, returns the FULL
// output (for searching multi-line output: adapted config, running config,
// site files).
func dexecShAll(container, sh string) string {
	out, err := exec.Command("docker", "exec", container, "sh", "-c", sh).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// dexecOK runs a shell command inside the container, returns success.
func dexecOK(container, sh string) bool {
	return exec.Command("docker", "exec", container, "sh", "-c", sh).Run() == nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
