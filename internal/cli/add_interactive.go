package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"

	"hemma/internal/config"
)

// Interactive `add service <name>` — the zero-flags form of cmdAdd. Like the
// update editor (update_interactive.go), it is strictly a flag collector: it
// gathers the field values and funnels through the exact same
// validate-before-persist path and single sync tail as the flags form
// (persistNewService), so the two entry points cannot diverge. It contains no
// mutation logic of its own.

// cmdAddInteractive runs the interactive editor for a new service. Reached
// only from cmdAdd when zero flags were given; TTY-gated so scripts that
// forgot their flags fail loud instead of hanging on a hidden prompt.
func cmdAddInteractive(repoRoot, cfgPath, name string) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		errf("no flags given and stdin is not a terminal — pass flags (see hemma help add)")
		return 2
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		errf("%v", err)
		return 1
	}
	if _, exists := cfg.Services[name]; exists {
		errf("Service %q already exists.", name)
		hint("Edit it with: hemma update service %s", name)
		return 1
	}
	// The pickers need something to pick from — refuse up front instead of
	// rendering a form that cannot validate (same preconditions the flags
	// form enforces, just before the editor rather than after).
	if len(cfg.DomainNames()) == 0 {
		errf("No domains defined yet — the fqdn could not match anything.")
		hint("Run 'hemma add domain <name>' first.")
		return 1
	}
	if len(cfg.Hosts) == 0 {
		errf("No hosts defined yet — the service needs a host to run on.")
		hint("Run 'hemma add host <name> <ip>' first.")
		return 1
	}

	// The auth_service must not gate itself (plan refuses it — a redirect
	// loop, design §4.5), so don't offer auth fields at all; a read-only note
	// replaces them.
	isAuthService := cfg.Defaults.AuthService != "" && name == cfg.Defaults.AuthService
	if isAuthService {
		fmt.Fprintf(os.Stderr, "Note: %q is the configured auth_service (the forward-auth backend) — auth mode/groups are not offered; gating the portal itself would create a redirect loop.\n", name)
	}

	// Field values the form collects. With a single domain the fqdn is
	// pre-filled to the obvious <name>.<domain> (Enter keeps it, editing is
	// free); with several domains guessing would be noise, so it starts empty.
	fqdn := ""
	if doms := cfg.DomainNames(); len(doms) == 1 {
		fqdn = name + "." + doms[0]
	}
	hosts := sortedKeysOf(cfg.Hosts)
	host := hosts[0]
	backend := ""
	mode := "none"
	var selGroups []string
	newGroup := ""

	// Group options come from reality (users db ∪ services.yaml), not from a
	// free-text field — see buildGroupOptions.
	userGroups, usersNote := loadUserGroups(repoRoot, cfg)
	opts := buildGroupOptions(userGroups, usersNote == "", cfg.Services)
	groupOpts := make([]huh.Option[string], 0, len(opts)+1)
	for _, o := range opts {
		groupOpts = append(groupOpts, huh.NewOption(o.Label, o.Name))
	}
	groupOpts = append(groupOpts, huh.NewOption("new group…", newGroupSentinel))
	groupsDesc := "Groups allowed access (OR'd). Members shown per group."
	if usersNote != "" {
		groupsDesc = "Groups allowed access (OR'd). Members unknown — " + usersNote + "."
	}

	validFQDN := func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return errors.New("the fqdn must not be empty")
		}
		if _, ok := cfg.MatchDomain(s); !ok {
			return fmt.Errorf("matches no defined domain (%s)", strings.Join(cfg.DomainNames(), ", "))
		}
		for n, svc := range cfg.Services {
			if svc.FQDN == s {
				return fmt.Errorf("already used by service %q", n)
			}
		}
		return nil
	}
	nonEmpty := func(what string) func(string) error {
		return func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New(what + " must not be empty")
			}
			return nil
		}
	}

	// One group = one page (see update_interactive.go): all fields render as a
	// single column; only the rare "new group" input is a follow-up page.
	fields := []huh.Field{
		huh.NewInput().Title("fqdn").Value(&fqdn).Validate(validFQDN),
		huh.NewSelect[string]().Title("host").Options(huh.NewOptions(hosts...)...).Value(&host),
		huh.NewInput().Title("backend").
			Description("reverse_proxy upstream, e.g. " + name + ":8080.").
			Value(&backend).Validate(nonEmpty("the backend")),
	}
	if !isAuthService {
		fields = append(fields,
			huh.NewSelect[string]().Title("auth mode").
				Options(huh.NewOptions("none", "forward", "oidc")...).
				Value(&mode),
			huh.NewMultiSelect[string]().Title("auth groups").
				Description(groupsDesc+" Ignored when auth mode is none.").
				Options(groupOpts...).
				Value(&selGroups),
		)
	}
	groups := []*huh.Group{huh.NewGroup(fields...)}
	if !isAuthService {
		groups = append(groups,
			huh.NewGroup(
				huh.NewInput().Title("new group name").
					Description("It has no members yet — 'hemma doctor' will flag it until a user carries it.").
					Value(&newGroup).
					Validate(nonEmpty("the group name")),
			).WithHideFunc(func() bool {
				return mode == "none" || !slices.Contains(selGroups, newGroupSentinel)
			}),
		)
	}

	// Render the form on stderr so stdout stays clean for the summary and the
	// sync report (same contract as the update editor).
	if err := huh.NewForm(groups...).WithOutput(os.Stderr).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "Aborted — nothing added.")
			return 0
		}
		errf("%v", err)
		return 1
	}

	svc := config.Service{
		FQDN:    strings.TrimSpace(fqdn),
		Host:    host,
		Backend: strings.TrimSpace(backend),
	}
	svc.Auth.Mode = config.AuthNone
	if !isAuthService && mode != "none" {
		svc.Auth.Mode = config.AuthMode(mode)
		var gs []string
		for _, g := range selGroups {
			if g == newGroupSentinel {
				g = strings.TrimSpace(newGroup)
			}
			if g != "" && !slices.Contains(gs, g) {
				gs = append(gs, g)
			}
		}
		sort.Strings(gs)
		svc.Auth.Groups = gs
	}

	for _, l := range summarizeNewService(svc) {
		fmt.Println(l)
	}
	return persistNewService(repoRoot, cfg, name, svc)
}

// summarizeNewService renders the "field: value" lines the add editor prints
// before persisting — the add-side sibling of summarizeServiceChanges (there
// is no old value to diff against). Auth lines only appear when a gate is set.
func summarizeNewService(svc config.Service) []string {
	lines := []string{
		"  fqdn: " + svc.FQDN,
		"  host: " + svc.Host,
		"  backend: " + svc.Backend,
	}
	if svc.Auth.Mode != config.AuthNone {
		lines = append(lines, "  auth mode: "+string(svc.Auth.Mode))
		gs := append([]string(nil), svc.Auth.Groups...)
		sort.Strings(gs)
		if len(gs) > 0 {
			lines = append(lines, "  auth groups: "+strings.Join(gs, ", "))
		}
	}
	return lines
}
