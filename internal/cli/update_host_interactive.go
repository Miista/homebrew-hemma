package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"

	"hemma/internal/config"
)

// Interactive `update host <name>` — the zero-flags form of hostUpdate,
// mirroring the service editor (update_interactive.go). Like that one it is
// strictly a field collector: it pre-fills the host's current values, lets the
// user edit them, and funnels through the same persist tail as the flags form
// (persistUpdatedHost), so validate-before-persist and the sync-mode rule
// cannot diverge between the two entry points. No mutation logic of its own.

// hostUpdateInteractive runs the interactive editor for one host. Reached only
// from hostUpdate when zero flags were given; TTY-gated so scripts that forgot
// their flags fail loud instead of hanging on a hidden prompt.
func hostUpdateInteractive(cfgPath, name string) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		errf("no flags given and stdin is not a terminal — pass flags (see hemma help update)")
		return 2
	}

	cfg, code := loadExisting(cfgPath, "update a host in")
	if cfg == nil {
		return code
	}
	h, exists := cfg.Hosts[name]
	if !exists {
		errf("Host %q does not exist.", name)
		return 1
	}

	// Editable values, pre-filled with current state (Enter keeps them).
	ip := h.IP
	sshDest := h.SSH
	// One entry per line reads better than a comma list for a reviewable trust
	// boundary, and matches how the service editor takes bypass paths.
	trusted := strings.Join(h.TrustedProxies, "\n")

	// Every declared host's ip is already trusted automatically (§4.7); say so,
	// so the field is not mistaken for the whole trust list.
	others := make([]string, 0, len(cfg.Hosts))
	for n, other := range cfg.Hosts {
		if n != name && other.IP != "" {
			others = append(others, other.IP)
		}
	}
	trustedDesc := "One IP or CIDR per line — extra proxies beyond the declared hosts (typically this host's cloudflared container)."
	if len(others) > 0 {
		trustedDesc += " Already trusted: this host plus " + strings.Join(others, ", ") + "."
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("ip").Value(&ip).Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if s == "" {
				return errors.New("the ip must not be empty")
			}
			if net.ParseIP(s) == nil {
				return errors.New("not a valid IP address")
			}
			// A LAN IP identifies exactly one host (same rule as add host).
			for n, other := range cfg.Hosts {
				if n != name && other.IP == s {
					return errors.New("already used by host " + n)
				}
			}
			return nil
		}),
		huh.NewInput().Title("ssh").
			Description("ssh(1) destination for 'hemma deploy'. Empty defaults to the host name.").
			Value(&sshDest),
		huh.NewText().Title("trusted proxies").
			Description(trustedDesc).
			Value(&trusted).
			Validate(func(s string) error {
				for _, e := range splitLines(s) {
					if strings.Contains(e, "/") {
						if _, _, err := net.ParseCIDR(e); err != nil {
							return errors.New(e + " is not a valid CIDR")
						}
					} else if net.ParseIP(e) == nil {
						return errors.New(e + " is not a valid IP address or CIDR")
					}
				}
				return nil
			}),
	))

	// Render on stderr so stdout stays clean for the summary and sync report
	// (same contract as the service editor and the create-user prompts).
	if err := form.WithTheme(formTheme()).WithOutput(os.Stderr).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "Aborted — no changes made.")
			return 0
		}
		errf("%v", err)
		return 1
	}

	updated := h
	updated.IP = strings.TrimSpace(ip)
	updated.SSH = strings.TrimSpace(sshDest)
	updated.TrustedProxies = splitLines(trusted)

	lines := summarizeHostChanges(h, updated)
	if len(lines) == 0 {
		fmt.Println("No changes.")
		return 0
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return persistUpdatedHost(cfgPath, cfg, name, updated)
}

// summarizeHostChanges renders one "field: old -> new" line per changed field,
// mirroring summarizeServiceChanges. Empty means nothing changed.
func summarizeHostChanges(oldHost, newHost config.Host) []string {
	var lines []string
	add := func(field, from, to string) {
		if from == to {
			return
		}
		if from == "" {
			from = "(unset)"
		}
		if to == "" {
			to = "(unset)"
		}
		lines = append(lines, fmt.Sprintf("  %s: %s -> %s", field, from, to))
	}
	add("ip", oldHost.IP, newHost.IP)
	add("ssh", oldHost.SSH, newHost.SSH)
	add("trusted_proxies", strings.Join(oldHost.TrustedProxies, ", "), strings.Join(newHost.TrustedProxies, ", "))
	return lines
}
