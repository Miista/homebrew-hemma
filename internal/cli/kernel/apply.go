package kernel

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
// caddy, and the tunnel half (restart cloudflared) only on a host with public
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
// the same rule: restart cloudflared unconditionally rather than trying to
// detect whether THIS run's generated config.yml differs from the previous one,
// matching how Caddy is reloaded every run regardless of whether its config
// actually changed.
func cmdApply(env *Env, repoRoot, cfgPath string, args []string) int {
	cfg, code := loadExisting(env, cfgPath, "apply")
	if cfg == nil {
		return code
	}

	// Refuse to make config live while the repo is drifted: applying would push
	// stale/incorrect generated files to pihole/caddy. The generated files on
	// disk are the source of truth for reload, so they must match services.yaml
	// first. This is the one command that hard-refuses on drift (design: apply
	// is the point of no return; everything else reports-but-proceeds).
	mf := loadManifest(env, repoRoot, cfg)
	if d := detectDrift(repoRoot, cfg, mf); d.Any() {
		errf(env, "Refusing to apply: repo is drifted (%d %s out of sync with services.yaml).",
			d.Count(), plural(d.Count(), "generated file"))
		printDriftDetail(env, d)
		fmt.Fprintln(env.Err)
		hint(env, "Run 'hemma doctor --fix' to reconcile the repo, then 'hemma apply' again.")
		return 1
	}

	self := localHost(cfg)
	if self == "" {
		errf(env, "This machine's IP matches no host in services.yaml — run apply on a managed host (the resolver or a service host).")
		return 1
	}
	fmt.Fprintf(env.Out, "Running on host %q.\n", self)

	isDNS := self == cfg.DNSHost()
	runsCaddy := hostRunsCaddy(cfg, self)
	runsTunnel := hostHasPublicService(repoRoot, cfg, self)

	if !isDNS && !runsCaddy {
		fmt.Fprintf(env.Out, "Nothing to apply here: %q is neither the resolver (%s) nor a service host.\n", self, cfg.DNSHost())
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
		fmt.Fprintf(env.Out, "\n%s== Validate (%s) ==%s\n", boldOn, self, boldOff)
		if runsCaddy {
			// hemma AUTHORED this host's trusted_proxies content (§4.7) — a
			// trust boundary derived from services.yaml, not user-supplied
			// content hemma merely relays like the (auth) snippet. An
			// unimported trusted-proxies file is therefore hemma asserting a
			// security posture that is not in effect, while every other signal
			// (sync succeeded, no drift, caddy validate passes — an unimported
			// file is not a Caddy error) says all is well. That silence is the
			// whole hazard, so it refuses HERE rather than only warning in
			// doctor: if hemma generates the file, hemma makes it count.
			if missing, err := caddyfileMissingTrustedProxiesImport(repoRoot, cfg, self); err != nil {
				fmt.Fprintf(env.Out, "  "+cross+" could not read this host's Caddyfile: %v\n", err)
				fmt.Fprintln(env.Out)
				errf(env, "Validation failed — nothing was restarted or reloaded.")
				return 1
			} else if missing {
				fmt.Fprintf(env.Out, "  "+cross+" Caddyfile does not import %s\n", render.TrustedProxiesFilename)
				fmt.Fprintf(env.Out, "      hemma generated this host's trusted_proxies config, but Caddy never loads it —\n")
				fmt.Fprintf(env.Out, "      client IPs would resolve to the nearest proxy, silently voiding any\n")
				fmt.Fprintf(env.Out, "      source-IP-based auth rule. Add it inside your global options block:\n")
				fmt.Fprintf(env.Out, "        {\n            servers {\n                import %s\n            }\n        }\n", render.TrustedProxiesFilename)
				fmt.Fprintln(env.Out)
				errf(env, "Validation failed — nothing was restarted or reloaded.")
				return 1
			}
			fmt.Fprintf(env.Out, "  "+tick+" Caddyfile imports %s\n", render.TrustedProxiesFilename)
			// Validate provisions the TLS app, so a missing cert fails HERE
			// rather than during the reload (verified: caddy v2.11 validate exit 1).
			if !runQuiet(env, "docker", "exec", caddyContainer, "caddy", "validate", "--config", cf, "--adapter", "caddyfile") {
				fmt.Fprintln(env.Out, "  "+cross+" caddy validate FAILED (missing cert or bad config?)")
				fmt.Fprintln(env.Out)
				errf(env, "Validation failed — nothing was restarted or reloaded.")
				return 1
			}
			fmt.Fprintln(env.Out, "  "+tick+" caddy validate passes")
		}
		if authValidate != nil {
			if !runQuiet(env, authValidate[0], authValidate[1:]...) {
				fmt.Fprintf(env.Out, "  "+cross+" %s config validate FAILED\n", authName)
				fmt.Fprintln(env.Out)
				errf(env, "Validation failed — nothing was restarted or reloaded.")
				return 1
			}
			fmt.Fprintf(env.Out, "  "+tick+" %s config validate passes\n", authName)
		}
	}

	// Phase 2: act. Only reached with every validation green.
	failed := 0

	if isDNS {
		fmt.Fprintf(env.Out, "\n%s== DNS (%s) ==%s\n", boldOn, self, boldOff)
		// pihole v6 does not reload conf-dir on reloaddns; a restart is required.
		if runQuiet(env, "docker", "restart", piholeContainer) {
			fmt.Fprintf(env.Out, "  "+tick+" restarted %s\n", piholeContainer)
		} else {
			fmt.Fprintf(env.Out, "  "+cross+" failed to restart %s\n", piholeContainer)
			failed++
		}
	}

	if runsCaddy {
		fmt.Fprintf(env.Out, "\n%s== Caddy (%s) ==%s\n", boldOn, self, boldOff)
		if runQuiet(env, "docker", "exec", caddyContainer, "caddy", "reload", "--config", cf) {
			fmt.Fprintln(env.Out, "  "+tick+" caddy reloaded")
		} else {
			fmt.Fprintln(env.Out, "  "+cross+" caddy reload FAILED")
			failed++
		}
	}

	if authReload != nil {
		fmt.Fprintf(env.Out, "\n%s== Auth (%s) ==%s\n", boldOn, authName, boldOff)
		if runQuiet(env, authReload[0], authReload[1:]...) {
			fmt.Fprintf(env.Out, "  "+tick+" %s reloaded\n", authName)
		} else {
			fmt.Fprintf(env.Out, "  "+cross+" %s reload FAILED\n", authName)
			failed++
		}
	}

	if runsTunnel {
		fmt.Fprintf(env.Out, "\n%s== Tunnel (%s) ==%s\n", boldOn, self, boldOff)
		// A plain restart is enough: cloudflared-wrapper reads config.yml fresh
		// on every process start (it has no live docker-events watch the way
		// gatus-wrapper does), and hemma writes that file directly rather than
		// via a container label — so there is no OTHER container that needs
		// touching for cloudflared to see the current ingress set, unlike the
		// label-based design this replaced (which would have needed every
		// labelled service AND cloudflared recreated on every apply).
		if runQuiet(env, "docker", "restart", cloudflaredContainer) {
			fmt.Fprintf(env.Out, "  "+tick+" restarted %s\n", cloudflaredContainer)
		} else {
			fmt.Fprintf(env.Out, "  "+cross+" failed to restart %s\n", cloudflaredContainer)
			failed++
		}
	}

	fmt.Fprintln(env.Out)
	if failed > 0 {
		errf(env, "%d %s failed.", failed, plural(failed, "step"))
		return 1
	}
	fmt.Fprintln(env.Out, "Applied.")
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

// caddyfileMissingTrustedProxiesImport reports whether host's Caddyfile in the
// checkout lacks the `import hemma.trusted_proxies.generated.caddy` line that
// puts hemma's generated trust boundary (§4.7) into effect. A Caddyfile that
// does not exist is NOT reported missing-the-import: there is no Caddy config
// for apply to make live in that case, and the reload step would fail on its
// own with a clearer error. Read-only, same repo path doctor checks — apply
// never edits the Caddyfile (the global options block it belongs in is
// hand-maintained; hemma only insists the line is there).
func caddyfileMissingTrustedProxiesImport(repoRoot string, cfg *config.Config, host string) (bool, error) {
	h, ok := cfg.Hosts[host]
	if !ok {
		return false, nil
	}
	cfPath := filepath.Join(repoRoot, h.ResolvedDir(host), config.DefaultCaddyDataDir, "Caddyfile")
	data, err := os.ReadFile(cfPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !strings.Contains(string(data), "import "+render.TrustedProxiesFilename), nil
}

// hostHasPublicService reports whether host has a cloudflared config.yml on
// disk — i.e. whether planCloudflaredConfig emitted one for it (it omits a
// host with no surviving public: true service rather than writing a
// catch-all-only file). apply has already refused above if the repo is
// drifted, so by this point the file's presence is a reliable proxy for
// "this host has at least one public service" without apply needing to know
// the plan package's internal synthetic-owner key format.
func hostHasPublicService(repoRoot string, cfg *config.Config, host string) bool {
	tunnelDir, ok := cfg.Defaults.ResolvedTunnelDir()
	if !ok {
		return false
	}
	hostM := cfg.Hosts[host]
	dir := filepath.Join(repoRoot, hostM.ResolvedDir(host), tunnelDir)
	_, err := os.Stat(filepath.Join(dir, render.CloudflaredConfigFilename))
	return err == nil
}

// runQuiet runs a command with its output captured, printing it (indented)
// only when the command fails. The happy path stays clean; on failure the
// user still sees the tool's own diagnostics (notably caddy's missing-cert
// error, which is why apply used to stream everything live).
func runQuiet(env *Env, name string, args ...string) bool {
	return runQuietIn(env, "", name, args...)
}

// runQuietIn is runQuiet with an explicit working directory, for a command
// whose behavior depends on cwd (e.g. a `docker compose` invocation, which
// resolves its project from the directory it's run in — unlike a plain
// `docker` command, which finds a container by name regardless of cwd). An
// empty dir behaves exactly like runQuiet (inherits the caller's cwd).
func runQuietIn(env *Env, dir, name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Fprintf(env.Err, "    %s\n", line)
	}
	return false
}
