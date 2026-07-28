package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/render"
)

// cmdApply makes the synced config live ON THE HOST IT RUNS ON.
//
//	hemma apply
//
// Like verify, apply is host-split: the DNS half (restart pihole) can only run
// on the resolver, the Caddy half (validate + reload) only on a host that runs
// caddy, and the tunnel half (recreate cloudflared) only on a host with public
// services. apply identifies which host it is via a local-IP match, then
// performs the parts it is responsible for. Run it on each affected host to
// make the whole change live — apply does not (and cannot) SSH elsewhere.
//
// The Caddy half runs `caddy validate` BEFORE `caddy reload`: validate provisions
// the TLS app, which loads cert files from disk, so a missing/wrong cert aborts
// here with a clear error instead of failing mid-reload. Command output (docker,
// caddy) is captured and shown only on failure — success prints just the ticks.
// reload is idempotent, so apply acts unconditionally on whatever this host owns
// (there is no "changed this run" notion outside sync) — the tunnel half follows
// the same rule: recreate cloudflared unconditionally rather than trying to
// detect whether THIS run's docker-compose.override.yml differs from the
// previous one, matching how Caddy is reloaded every run regardless of whether
// its config actually changed.
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
		hint("Run 'hemma doctor --fix' to reconcile the repo, then 'hemma apply' again.")
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
	runsTunnel := hostHasPublicService(repoRoot, cfg, self)

	if !isDNS && !runsCaddy {
		fmt.Printf("Nothing to apply here: %q is neither the resolver (%s) nor a service host.\n", self, cfg.DNSHost())
		return 0
	}

	// Resolve the auth half up front so validation can run before ANY restart.
	var authName string
	var authValidate, authReload []string
	if name := cfg.Defaults.AuthService; name != "" {
		if s, ok := cfg.Services[name]; ok && s.Host == self && !s.Disabled {
			validate, reload := auth.Default().ApplyCommands(name)
			if reload != nil {
				authName, authValidate, authReload = name, validate, reload
			}
		}
	}

	const cf = "/etc/caddy/Caddyfile"

	// Phase 1: validate everything this host owns BEFORE restarting anything.
	// A bad Caddyfile or auth config must not cost a pihole restart (DNS blip)
	// or leave the host half-applied — validation failures abort the whole
	// apply with nothing touched.
	if runsCaddy || authValidate != nil {
		fmt.Printf("\n%s== Validate (%s) ==%s\n", boldOn, self, boldOff)
		if runsCaddy {
			// Validate provisions the TLS app, so a missing cert fails HERE
			// rather than during the reload (verified: caddy v2.11 validate exit 1).
			if !runQuiet("docker", "exec", caddyContainer, "caddy", "validate", "--config", cf, "--adapter", "caddyfile") {
				fmt.Println("  " + cross + " caddy validate FAILED (missing cert or bad config?)")
				fmt.Println()
				errf("Validation failed — nothing was restarted or reloaded.")
				return 1
			}
			fmt.Println("  " + tick + " caddy validate passes")
		}
		if authValidate != nil {
			if !runQuiet(authValidate[0], authValidate[1:]...) {
				fmt.Printf("  "+cross+" %s config validate FAILED\n", authName)
				fmt.Println()
				errf("Validation failed — nothing was restarted or reloaded.")
				return 1
			}
			fmt.Printf("  "+tick+" %s config validate passes\n", authName)
		}
	}

	// Phase 2: act. Only reached with every validation green.
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
		if runQuiet("docker", "exec", caddyContainer, "caddy", "reload", "--config", cf) {
			fmt.Println("  " + tick + " caddy reloaded")
		} else {
			fmt.Println("  " + cross + " caddy reload FAILED")
			failed++
		}
	}

	if authReload != nil {
		fmt.Printf("\n%s== Auth (%s) ==%s\n", boldOn, authName, boldOff)
		if runQuiet(authReload[0], authReload[1:]...) {
			fmt.Printf("  "+tick+" %s reloaded\n", authName)
		} else {
			fmt.Printf("  "+cross+" %s reload FAILED\n", authName)
			failed++
		}
	}

	if runsTunnel {
		fmt.Printf("\n%s== Tunnel (%s) ==%s\n", boldOn, self, boldOff)
		// A plain restart is enough: cloudflared-wrapper reads config.yml fresh
		// on every process start (it has no live docker-events watch the way
		// gatus-wrapper does), and hemma writes that file directly rather than
		// via a container label — so there is no OTHER container that needs
		// touching for cloudflared to see the current ingress set, unlike the
		// label-based design this replaced (which would have needed every
		// labelled service AND cloudflared recreated on every apply).
		if runQuiet("docker", "restart", cloudflaredContainer) {
			fmt.Printf("  "+tick+" restarted %s\n", cloudflaredContainer)
		} else {
			fmt.Printf("  "+cross+" failed to restart %s\n", cloudflaredContainer)
			failed++
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

// hostHasPublicService reports whether host has a cloudflared config.yml on
// disk — i.e. whether planCloudflaredConfig emitted one for it (it omits a
// host with no surviving public: true service rather than writing a
// catch-all-only file). apply has already refused above if the repo is
// drifted, so by this point the file's presence is a reliable proxy for
// "this host has at least one public service" without apply needing to know
// the plan package's internal synthetic-owner key format.
func hostHasPublicService(repoRoot string, cfg *config.Config, host string) bool {
	hostM := cfg.Hosts[host]
	dir := filepath.Join(repoRoot, hostM.ResolvedDir(host), hostM.ResolvedTunnelDir(cfg.Defaults))
	_, err := os.Stat(filepath.Join(dir, render.CloudflaredConfigFilename))
	return err == nil
}

// runQuiet runs a command with its output captured, printing it (indented)
// only when the command fails. The happy path stays clean; on failure the
// user still sees the tool's own diagnostics (notably caddy's missing-cert
// error, which is why apply used to stream everything live).
func runQuiet(name string, args ...string) bool {
	return runQuietIn("", name, args...)
}

// runQuietIn is runQuiet with an explicit working directory, for a command
// whose behavior depends on cwd (e.g. a `docker compose` invocation, which
// resolves its project from the directory it's run in — unlike a plain
// `docker` command, which finds a container by name regardless of cwd). An
// empty dir behaves exactly like runQuiet (inherits the caller's cwd).
func runQuietIn(dir, name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "    %s\n", line)
	}
	return false
}
