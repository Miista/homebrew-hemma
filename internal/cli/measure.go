package cli

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"splitdns/internal/config"
)

// measureScript is the user's measure.sh (made portable: awk not bc; curl's own
// %{remote_ip} for the resolve line; -4 to skip the suppressed-AAAA path;
// optional $2 pin via --resolve for the A/B legs). Embedded so the single splitdns
// binary carries it. Requires bash, curl, awk (universal on the hosts).
//
//go:embed measure.sh
var measureScript string

// cmdMeasure times the HTTPS request breakdown for a service via the embedded
// measure.sh.
//
//	splitdns measure <service|fqdn>              measure the current path (full breakdown, incl. DNS)
//	splitdns measure --compare <service|fqdn>    A/B split-horizon vs public (dns-host only)
//
// Plain measure resolves naturally — on the LAN that is the split-horizon
// record — and includes the real DNS lookup time. Read-only.
//
// --compare measures both the internal and public paths by pinning each with
// curl --resolve (split IP from config, public IP via DoH), so it is fully
// read-only — no pihole toggle, no mutation. Pinning skips DNS, so the A/B omits
// the dns metric (immaterial: ~2ms of an ~80ms request). It is gated to the
// dns-host because DoH egress is only sanctioned from the resolver on this
// network — the public-IP lookup must happen there.
func cmdMeasure(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	compare := fs.Bool("compare", false, "A/B split-horizon vs public via --resolve (dns-host only; read-only)")
	fs.BoolVar(compare, "ab", false, "alias for --compare")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		errf("Missing the <service> or <fqdn> to measure.")
		hint("Usage: splitdns measure [--compare] <service|fqdn>")
		return 2
	}

	cfg, code := loadExisting(cfgPath, "measure")
	if cfg == nil {
		return code
	}

	name, svc, ok := resolveService(cfg, rest[0])
	if !ok {
		errf("No service named %q and no service with fqdn %q in services.yaml.", rest[0], rest[0])
		return 1
	}
	if svc.Disabled {
		errf("Service %q is disabled — enable it before measuring.", name)
		return 1
	}
	url := "https://" + svc.FQDN + "/"

	if !*compare {
		if err := runMeasureScript(url, ""); err != nil {
			errf("%v", err)
			return 1
		}
		return 0
	}

	// --compare: read-only A/B via --resolve. Gate on the dns-host (DoH egress
	// is only sanctioned from the resolver here).
	if localHost(cfg) != cfg.DNSHost() {
		errf("--compare must run on the dns-host (%s): the public-IP lookup uses DoH, which only the resolver may reach on this network.", cfg.DNSHost())
		hint("Run 'splitdns measure %s' here for a single (split-horizon) measurement, or run --compare on %s.", rest[0], cfg.DNSHost())
		return 1
	}

	splitIP := cfg.Hosts[svc.Host].IP
	if splitIP == "" {
		errf("Service %q host %q has no IP in services.yaml.", name, svc.Host)
		return 1
	}
	publicIP, err := lookupPublicIP(svc.FQDN)
	if err != nil {
		errf("could not look up the public IP for %s via DoH: %v", svc.FQDN, err)
		return 1
	}

	fmt.Printf("%s== A: split-horizon (%s) ==%s\n", boldOn, splitIP, boldOff)
	if err := runMeasureScript(url, splitIP); err != nil {
		errf("split-horizon leg failed: %v", err)
		return 1
	}
	fmt.Printf("\n%s== B: public (%s) ==%s\n", boldOn, publicIP, boldOff)
	if err := runMeasureScript(url, publicIP); err != nil {
		errf("public leg failed: %v", err)
		return 1
	}
	fmt.Println("\nCompare the two blocks above: A (split-horizon) vs B (public). DNS excluded (pinned).")
	return 0
}

// runMeasureScript runs the embedded measure.sh against url via `bash -s`,
// passing an optional pin IP (empty = natural resolution). Output streams live.
func runMeasureScript(url, pinIP string) error {
	cmd := exec.Command("bash", "-s", url, pinIP)
	cmd.Stdin = strings.NewReader(measureScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("measure script failed for %s: %w", url, err)
	}
	return nil
}

// lookupPublicIP resolves fqdn's public IPv4 via Cloudflare DoH over HTTPS.
// Used only by --compare on the dns-host, where DoH egress is allowed. Returns
// the first A record.
func lookupPublicIP(fqdn string) (string, error) {
	out, err := exec.Command("curl", "-s", "--max-time", "8",
		"-H", "accept: application/dns-json",
		"https://cloudflare-dns.com/dns-query?name="+fqdn+"&type=A").Output()
	if err != nil {
		return "", err
	}
	var resp struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parsing DoH response: %w", err)
	}
	for _, a := range resp.Answer {
		if a.Type == 1 { // A record
			return a.Data, nil
		}
	}
	return "", fmt.Errorf("no public A record found")
}

// resolveService looks up a service by its name first, then by fqdn.
func resolveService(cfg *config.Config, arg string) (string, config.Service, bool) {
	if svc, ok := cfg.Services[arg]; ok {
		return arg, svc, true
	}
	if name := serviceByFQDN(cfg, arg); name != "" {
		return name, cfg.Services[name], true
	}
	return "", config.Service{}, false
}
