package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"splitdns/internal/auth"
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
// here with a clear error instead of failing mid-reload. Command output (docker,
// caddy) is captured and shown only on failure — success prints just the ticks.
// reload is idempotent, so apply acts unconditionally on whatever this host owns
// (there is no "changed this run" notion outside sync).
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
		if runQuiet("docker", "restart", piholeContainer) {
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
		if !runQuiet("docker", "exec", caddyContainer, "caddy", "validate", "--config", cf, "--adapter", "caddyfile") {
			fmt.Println("  " + cross + " caddy validate FAILED — not reloading (missing cert or bad config?)")
			failed++
		} else {
			fmt.Println("  " + tick + " caddy validate passes")
			if runQuiet("docker", "exec", caddyContainer, "caddy", "reload", "--config", cf) {
				fmt.Println("  " + tick + " caddy reloaded")
			} else {
				fmt.Println("  " + cross + " caddy reload FAILED")
				failed++
			}
		}
	}

	// Auth provider half: runs only on the host that runs the auth_service.
	// Same validate-before-reload discipline as caddy; the provider supplies
	// the commands (Authelia: config validate, then container restart — it has
	// no hot reload).
	if name := cfg.Defaults.AuthService; name != "" {
		if s, ok := cfg.Services[name]; ok && s.Host == self && !s.Disabled {
			provider := auth.Default()
			validate, reload := provider.ApplyCommands(name)
			if reload != nil {
				fmt.Printf("\n%s== Auth (%s) ==%s\n", boldOn, name, boldOff)
				valOK := true
				if validate != nil {
					if runQuiet(validate[0], validate[1:]...) {
						fmt.Printf("  "+tick+" %s config validate passes\n", provider.Name())
					} else {
						fmt.Printf("  "+cross+" %s config validate FAILED — not reloading\n", provider.Name())
						failed++
						valOK = false
					}
				}
				if valOK {
					if runQuiet(reload[0], reload[1:]...) {
						fmt.Printf("  "+tick+" %s reloaded\n", name)
					} else {
						fmt.Printf("  "+cross+" %s reload FAILED\n", name)
						failed++
					}
				}
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

// runQuiet runs a command with its output captured, printing it (indented)
// only when the command fails. The happy path stays clean; on failure the
// user still sees the tool's own diagnostics (notably caddy's missing-cert
// error, which is why apply used to stream everything live).
func runQuiet(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "    %s\n", line)
	}
	return false
}
