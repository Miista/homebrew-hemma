package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"hemma/internal/config"
)

// checkDeployReadiness audits, LOCALLY, whether `hemma deploy` could reach
// every remote target from this machine: the host key must already be in
// known_hosts. deploy sshes with BatchMode=yes, which cannot answer the
// interactive trust prompt, so an unknown host key fails deploy outright.
// Checked with `ssh-keygen -F` against both the host's ip and its ssh
// destination (either satisfies ssh's lookup); -F understands hashed entries
// where a grep would not.
//
// This check never connects — doctor stays inside the no-SSH boundary
// (design §12; deploy is the ONE exception, and its phase-0 probe is where
// "can we actually connect" is verified for real). It is not --fix-able:
// hemma does not own known_hosts, so the recipe is to connect once
// interactively, so a human sees and accepts the fingerprint (deliberately
// NOT ssh-keyscan, which would trust whatever key is on the wire unseen).
//
// The target set mirrors deploy's default (every host with a role); self is
// skipped (deploy runs locally there, no ssh). Returns the number of hosts
// missing a known_hosts entry.
func checkDeployReadiness(cfg *config.Config) int {
	self := localHost(cfg)
	// Readiness is a question about the deploy ORIGIN, not about every host.
	// When defaults.deploy_host names one, only that host is audited: elsewhere
	// the finding would be unfixable by design (a segmented network may give a
	// non-origin host no route to its peers at all), and an unfixable finding
	// that fails doctor forever teaches people to ignore doctor. Unset keeps the
	// old behaviour of checking everywhere, so adopting the field is opt-in.
	if dh := cfg.Defaults.DeployHost; dh != "" && self != dh {
		return 0
	}
	targets, err := resolveDeployTargets(cfg, self, nil)
	if err != nil {
		return 0 // config-level problems are reported elsewhere
	}
	problems := 0
	remotes := 0
	for _, t := range targets {
		if t.Local {
			continue
		}
		remotes++
		ip := cfg.Hosts[t.Name].IP
		if hostKnown(ip) || hostKnown(sshHostPart(t.Dest)) {
			continue
		}
		problems++
		fmt.Printf("%s %s: no known_hosts entry for its deploy destination (%s)\n", warn, t.Name, t.Dest)
		fmt.Printf("  'hemma deploy' uses 'ssh -o BatchMode=yes' and will fail host-key verification.\n")
		fmt.Printf("  fix: connect once interactively to accept and record the host key —\n")
		fmt.Printf("       ssh %s\n", t.Dest)
		fmt.Printf("       (verify the fingerprint before accepting.)\n")
	}
	if remotes > 0 && problems == 0 {
		fmt.Printf(tick+" Deploy readiness: %d remote %s in known_hosts.\n", remotes, plural(remotes, "host"))
	}
	return problems
}

// hostKnown reports whether ssh already trusts a host key for the given name
// (ssh-keygen -F searches the default known_hosts set, hashed entries
// included). Overridable in tests. An empty name is never known.
var hostKnown = func(host string) bool {
	if host == "" {
		return false
	}
	return exec.Command("ssh-keygen", "-F", host).Run() == nil
}

// sshHostPart extracts the host from a verbatim ssh destination for the
// known_hosts lookup: `user@host` → `host`. An ssh_config alias passes
// through unchanged (ssh records the key under the name it connected with
// only when no HostName rewrite applies — a miss here is why hostKnown also
// tries the ip).
func sshHostPart(dest string) string {
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[i+1:]
	}
	return dest
}
