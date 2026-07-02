package cli

import (
	"fmt"
	"os"
	"os/exec"

	"splitdns/internal/config"
)

// cmdApply makes the synced config live ON THE HOST IT RUNS ON.
//
//	splitdns apply
//
// Like verify, apply is host-split: the DNS half (restart pihole) can only run
// on the resolver, the Caddy half (validate + reload) only on a host that runs
// caddy. apply identifies which host it is via a local-IP match, then performs
// the half (or halves) it is responsible for. Run it on each affected host to
// make the whole change live — apply does not (and cannot) SSH elsewhere.
//
// The Caddy half runs `caddy validate` BEFORE `caddy reload`: validate provisions
// the TLS app, which loads cert files from disk, so a missing/wrong cert aborts
// here with a clear error instead of failing mid-reload. reload is idempotent, so
// apply acts unconditionally on whatever this host owns (there is no "changed
// this run" notion outside sync).
func cmdApply(repoRoot, cfgPath string, args []string) int {
	cfg, code := loadExisting(cfgPath, "apply")
	if cfg == nil {
		return code
	}

	// Refuse to make config live while the repo is drifted: applying would push
	// stale/incorrect generated files to pihole/caddy. The generated files on
	// disk are the source of truth for reload, so they must match services.yaml
	// first. This is the one command that hard-refuses on drift (design: apply
	// is the point of no return; everything else reports-but-proceeds).
	mf := loadManifest(repoRoot, cfg)
	if d := detectDrift(repoRoot, cfg, mf); d.Any() {
		errf("Refusing to apply: repo is drifted (%d %s out of sync with services.yaml).",
			d.Count(), plural(d.Count(), "generated file"))
		printDriftDetail(d)
		fmt.Fprintln(os.Stderr)
		hint("Run 'splitdns doctor --fix' to reconcile the repo, then 'splitdns apply' again.")
		return 1
	}

	self := localHost(cfg)
	if self == "" {
		errf("This machine's IP matches no host in services.yaml — run apply on a managed host (the resolver or a service host).")
		return 1
	}
	fmt.Printf("Running on host %q.\n", self)

	isDNS := self == cfg.DNSHost()
	runsCaddy := hostRunsCaddy(cfg, self)

	if !isDNS && !runsCaddy {
		fmt.Printf("Nothing to apply here: %q is neither the resolver (%s) nor a service host.\n", self, cfg.DNSHost())
		return 0
	}

	failed := 0

	if isDNS {
		fmt.Printf("\n%s== DNS (%s) ==%s\n", boldOn, self, boldOff)
		// pihole v6 does not reload conf-dir on reloaddns; a restart is required.
		if runLive("docker", "restart", piholeContainer) {
			fmt.Printf("  "+tick+" restarted %s\n", piholeContainer)
		} else {
			fmt.Printf("  "+cross+" failed to restart %s\n", piholeContainer)
			failed++
		}
	}

	if runsCaddy {
		fmt.Printf("\n%s== Caddy (%s) ==%s\n", boldOn, self, boldOff)
		const cf = "/etc/caddy/Caddyfile"
		// Validate first — provisions the TLS app, so a missing cert fails HERE
		// rather than during the reload (verified: caddy v2.11 validate exit 1).
		if !runLive("docker", "exec", caddyContainer, "caddy", "validate", "--config", cf, "--adapter", "caddyfile") {
			fmt.Println("  " + cross + " caddy validate FAILED — not reloading (missing cert or bad config?)")
			failed++
		} else {
			fmt.Println("  " + tick + " caddy validate passes")
			if runLive("docker", "exec", caddyContainer, "caddy", "reload", "--config", cf) {
				fmt.Println("  " + tick + " caddy reloaded")
			} else {
				fmt.Println("  " + cross + " caddy reload FAILED")
				failed++
			}
		}
	}

	fmt.Println()
	if failed > 0 {
		errf("%d %s failed.", failed, plural(failed, "step"))
		return 1
	}
	fmt.Println("Applied.")
	return 0
}

// hostRunsCaddy reports whether host serves any (non-disabled) service, i.e. a
// caddy site is generated for it and the caddy container should be reloaded.
func hostRunsCaddy(cfg *config.Config, host string) bool {
	for _, s := range cfg.Services {
		if s.Host == host && !s.Disabled {
			return true
		}
	}
	return false
}

// runLive runs a command with its output streamed to the user's terminal and
// reports success. Used for the docker restart/validate/reload steps so the
// user sees caddy's own diagnostics (notably the missing-cert error).
func runLive(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}
