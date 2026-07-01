package cli

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"sd/internal/config"
	"sd/internal/plan"
	syncpkg "sd/internal/sync"
)

// measureRuns is the number of timed requests per leg (after warmup).
const measureRuns = 5

// cmdMeasure times the HTTPS request breakdown (dns/connect/tls/ttfb/total) for
// a service, from the vantage point of the host it runs on.
//
//	sd measure <service|fqdn>              measure the current path (read-only)
//	sd measure --compare <service|fqdn>    A/B: split-horizon vs public (dns-host only)
//
// Default is a single, read-only measurement of whatever the local resolver
// currently returns — on the LAN that is the split-horizon record, so it times
// the internal path.
//
// --compare performs the split-vs-public comparison. Because all DNS is forced
// through pihole (only the resolver may reach upstream), the ONLY way to reach
// the public answer from inside the LAN is to make pihole forward upstream —
// i.e. temporarily remove the split-horizon record. So --compare:
//
//  1. measure with the record present            (split-horizon / internal)
//  2. disable the service + restart pihole        (record gone → pihole forwards upstream)
//  3. measure with the record absent             (public / upstream)
//  4. restore: re-enable + restart pihole
//
// That mutates live DNS and restarts pihole twice, so it is opt-in AND only
// runnable on the dns-host (the one machine that owns the record + can restart
// pihole). The service host / caddy is irrelevant to the A/B — only which
// answer pihole gives changes. Restore is deferred so an interrupt still undoes
// the toggle.
//
// Vantage caveat: --compare measures FROM the dns-host, which is not where the
// split-horizon win is largest — a workstation on a client VLAN sees a bigger
// gap (its internal path skips the Cloudflare round-trip entirely). For
// client-perceived numbers, run plain `sd measure` from that workstation while
// the record is toggled. The dns-host A/B is a quick sanity check, not the
// client's-eye view.
func cmdMeasure(repoRoot, cfgPath string, args []string) int {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	compare := fs.Bool("compare", false, "A/B compare split-horizon vs public (dns-host only; toggles the record)")
	fs.BoolVar(compare, "ab", false, "alias for --compare")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		errf("Missing the <service> or <fqdn> to measure.")
		hint("Usage: sd measure [--compare] <service|fqdn>")
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
		fmt.Printf("Measuring %s (%d runs)...\n", url, measureRuns)
		st, err := measureURL(url)
		if err != nil {
			errf("%v", err)
			return 1
		}
		st.print("current path")
		return 0
	}

	// --compare: A/B toggle. Gate on the dns-host.
	self := localHost(cfg)
	if self != cfg.DNSHost() {
		errf("--compare must run on the dns-host (%s): it toggles the split-horizon record and restarts pihole, which only the resolver can do.", cfg.DNSHost())
		hint("Run 'sd measure %s' here for a single (split-horizon) measurement, or run 'sd measure --compare %s' on %s.", rest[0], rest[0], cfg.DNSHost())
		return 1
	}

	return runCompare(repoRoot, cfg, name, url)
}

// runCompare performs the split-vs-public A/B on the dns-host. It is the only
// mutating path in measure: it removes the split-horizon record (by disabling
// the service), restarts pihole, measures the public leg, then always restores.
func runCompare(repoRoot string, cfg *config.Config, name, url string) int {
	fmt.Printf("A/B measuring %s (%d runs each leg).\n", url, measureRuns)
	fmt.Println("This toggles the split-horizon DNS record and restarts pihole twice; it will be restored.")

	// Leg A: split-horizon (record present, as-is).
	fmt.Printf("\n%s== A: split-horizon ==%s\n", boldOn, boldOff)
	splitStats, err := measureURL(url)
	if err != nil {
		errf("split-horizon leg failed: %v", err)
		return 1
	}
	splitStats.print("split-horizon")

	// Disable the service → its DNS record is deleted → restart pihole so it
	// forwards upstream for this fqdn. Restore is deferred immediately after we
	// commit to the mutation, so an interrupt (Ctrl-C) still re-enables.
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		fmt.Printf("\n%s== restoring split-horizon record ==%s\n", boldOn, boldOff)
		if err := setDisabled(repoRoot, cfg, name, false); err != nil {
			errf("RESTORE FAILED for %q: %v", name, err)
			hint("Re-enable manually: sd enable service %s && sd apply", name)
			return
		}
		if !runLive("docker", "restart", piholeContainer) {
			errf("RESTORE: pihole restart failed — run 'sd apply' on %s.", cfg.DNSHost())
			return
		}
		fmt.Printf("  "+tick+" re-enabled %q and restarted pihole\n", name)
	}
	// Guarantee restore on Ctrl-C / SIGTERM as well as normal return.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		restore()
		os.Exit(130)
	}()
	defer restore()

	fmt.Printf("\n%s== switching to public path ==%s\n", boldOn, boldOff)
	if err := setDisabled(repoRoot, cfg, name, true); err != nil {
		errf("failed to disable %q: %v", name, err)
		return 1
	}
	if !runLive("docker", "restart", piholeContainer) {
		errf("pihole restart failed after disabling; aborting.")
		return 1
	}
	fmt.Println("  " + tick + " removed split-horizon record, pihole now forwards upstream")

	// Leg B: public (record absent → pihole forwards upstream to the public IP).
	fmt.Printf("\n%s== B: public ==%s\n", boldOn, boldOff)
	pubStats, err := measureURL(url)
	if err != nil {
		errf("public leg failed: %v (the domain may have no public record)", err)
		return 1
	}
	pubStats.print("public")

	// Restore now (deferred restore becomes a no-op).
	restore()

	printDelta(splitStats, pubStats)
	return 0
}

// setDisabled flips a service's Disabled flag, persists, and reconciles the
// generated files (delete on disable, regenerate on enable) so the DNS record
// is dropped/recreated. Mirrors cmdEnableDisable but without CLI framing.
func setDisabled(repoRoot string, cfg *config.Config, name string, disabled bool) error {
	svc := cfg.Services[name]
	svc.Disabled = disabled
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		return err
	}
	mf := loadManifest(repoRoot, cfg)
	eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
	if disabled {
		_, err := eng.RemoveService(name)
		return err
	}
	_, err := eng.Reconcile(plan.Build(cfg), syncpkg.Incremental)
	return err
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

// --- timing ---

// timingStats holds per-metric samples (milliseconds) across the runs.
type timingStats struct {
	dns, connect, tls, ttfb, total []float64
}

// measureURL warms up then runs measureRuns timed requests via curl, capturing
// curl's own phase breakdown. We shell out to curl rather than reimplement in Go
// because curl is present on every host, verify.go already uses it, and Go's
// resolver behaves inconsistently across platforms (notably slow/serialized DNS
// on macOS and the pure-Go resolver on Linux), which would make the numbers
// non-comparable. curl's time_* fields are exactly what the user's measure.sh
// reports and trusts. -k skips cert verification (we measure timing, not trust).
func measureURL(url string) (*timingStats, error) {
	// Warmup (not measured) to prime DNS cache / backend.
	for i := 0; i < 3; i++ {
		if _, err := curlTimed(url); err != nil {
			return nil, fmt.Errorf("warmup request to %s failed: %w", url, err)
		}
	}

	st := &timingStats{}
	for i := 0; i < measureRuns; i++ {
		t, err := curlTimed(url)
		if err != nil {
			return nil, fmt.Errorf("request to %s failed: %w", url, err)
		}
		st.dns = append(st.dns, t.dns)
		st.connect = append(st.connect, t.connect)
		st.tls = append(st.tls, t.tls)
		st.ttfb = append(st.ttfb, t.ttfb)
		st.total = append(st.total, t.total)
		fmt.Printf("  run %d: dns=%.0fms connect=%.0fms tls=%.0fms ttfb=%.0fms total=%.0fms\n",
			i+1, t.dns, t.connect, t.tls, t.ttfb, t.total)
	}
	return st, nil
}

// oneTiming holds a single request's cumulative timings (ms from request start).
type oneTiming struct{ dns, connect, tls, ttfb, total float64 }

// curlTimed runs one curl request and parses its timing breakdown into ms.
// The -w fields are cumulative-from-start, matching curl's semantics and the
// user's measure.sh: namelookup ≤ connect ≤ appconnect(tls) ≤ starttransfer(ttfb) ≤ total.
func curlTimed(url string) (oneTiming, error) {
	var t oneTiming
	const wfmt = "%{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total}"
	out, err := exec.Command("curl", "-sk", "-o", "/dev/null", "-m", "15", "-w", wfmt, url).Output()
	if err != nil {
		return t, err
	}
	var dns, connect, appconnect, ttfb, total float64
	if _, err := fmt.Sscanf(string(out), "%f %f %f %f %f", &dns, &connect, &appconnect, &ttfb, &total); err != nil {
		return t, fmt.Errorf("could not parse curl timing %q: %w", string(out), err)
	}
	// curl reports seconds; convert to ms.
	t = oneTiming{dns: dns * 1000, connect: connect * 1000, tls: appconnect * 1000, ttfb: ttfb * 1000, total: total * 1000}
	return t, nil
}

// --- output ---

func (st *timingStats) print(label string) {
	fmt.Printf("\n%-10s  %s\n", label, "mean / sd / min / max (ms)")
	row := func(name string, xs []float64) {
		mean, sd, min, max := stat(xs)
		fmt.Printf("%-10s  mean=%5.0f  sd=%4.0f  min=%5.0f  max=%5.0f\n", name, mean, sd, min, max)
	}
	row("dns", st.dns)
	row("connect", st.connect)
	row("tls", st.tls)
	row("ttfb", st.ttfb)
	row("total", st.total)
}

// printDelta shows the split-vs-public comparison on the headline metrics.
func printDelta(split, pub *timingStats) {
	fmt.Printf("\n%s== A/B delta (public − split) ==%s\n", boldOn, boldOff)
	cmp := func(name string, a, b []float64) {
		am, _, _, _ := stat(a)
		bm, _, _, _ := stat(b)
		d := bm - am
		factor := ""
		if am > 0 {
			factor = fmt.Sprintf("  (%.1f×)", bm/am)
		}
		fmt.Printf("%-10s  split=%5.0fms  public=%5.0fms  Δ=%+5.0fms%s\n", name, am, bm, d, factor)
	}
	cmp("connect", split.connect, pub.connect)
	cmp("tls", split.tls, pub.tls)
	cmp("ttfb", split.ttfb, pub.ttfb)
	cmp("total", split.total, pub.total)
}

// stat returns mean, population std-dev, min, max of xs (0s if empty).
func stat(xs []float64) (mean, sd, min, max float64) {
	if len(xs) == 0 {
		return
	}
	min, max = xs[0], xs[0]
	var sum float64
	for _, x := range xs {
		sum += x
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	mean = sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	sd = math.Sqrt(sq / float64(len(xs)))
	return
}
