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
	syncpkg "hemma/internal/sync"
)

// Interactive `update service <name>` — the zero-flags form of cmdUpdate.
// The editor is strictly a flag collector: it pre-fills the service's current
// values, lets the user edit them, and then funnels through the exact same
// persist path as the flags form (persistUpdatedService), so the
// validate-before-persist rule and the single sync tail (design §6.1) cannot
// diverge between the two entry points. It contains no mutation logic of its
// own.

// newGroupSentinel is the multi-select value of the "new group…" escape
// hatch. NUL can never be a real group name (YAML plain scalars can't carry
// it), so it can't collide.
const newGroupSentinel = "\x00new-group"

// cmdUpdateInteractive runs the interactive editor for one service. Reached
// only from cmdUpdate when zero flags were given; TTY-gated so scripts that
// forgot their flags fail loud instead of hanging on a hidden prompt.
func cmdUpdateInteractive(repoRoot, cfgPath, name string) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		errf("no flags given and stdin is not a terminal — pass flags (see hemma help update)")
		return 2
	}

	cfg, code := loadExisting(cfgPath, "update")
	if cfg == nil {
		return code
	}
	svc, exists := cfg.Services[name]
	if !exists {
		errf("Service %q does not exist.", name)
		return 1
	}

	// The auth_service must not gate itself (plan refuses it — a redirect
	// loop, design §4.5), so don't offer auth fields at all; a read-only note
	// replaces them.
	isAuthService := cfg.Defaults.AuthService != "" && name == cfg.Defaults.AuthService
	if isAuthService {
		fmt.Fprintf(os.Stderr, "Note: %q is the auth_service (the forward-auth backend) — auth mode/groups are not editable; gating the portal itself would create a redirect loop.\n", name)
	}

	// Editable values, pre-filled with current state (Enter keeps them).
	fqdn := svc.FQDN
	host := svc.Host
	backend := svc.Backend
	mode := string(svc.Auth.Mode)
	if mode == "" {
		mode = "none"
	}
	selGroups := append([]string(nil), svc.Auth.Groups...)
	newGroup := ""
	public := svc.Public
	disabled := svc.Disabled
	bypass := strings.Join(svc.Auth.BypassPaths, "\n")

	hosts := sortedKeysOf(cfg.Hosts)
	if _, ok := cfg.Hosts[host]; !ok && host != "" {
		// The service references a host that is no longer declared; keep it
		// selectable so Enter-through preserves the current value.
		hosts = append(hosts, host)
		sort.Strings(hosts)
	}

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

	nonEmpty := func(what string) func(string) error {
		return func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New(what + " must not be empty")
			}
			return nil
		}
	}

	// One group = one page: huh paginates per group, so splitting fields
	// across groups strands each on a mostly-empty screen. All fields render
	// as a single column; only the rare "new group" input is a follow-up page.
	fields := []huh.Field{
		huh.NewInput().Title("fqdn").Value(&fqdn).Validate(nonEmpty("the fqdn")),
		huh.NewSelect[string]().Title("host").Options(huh.NewOptions(hosts...)...).Value(&host),
		huh.NewInput().Title("backend").Value(&backend),
		huh.NewConfirm().Title("public").
			Description("Reachable from the internet too? Records intent only — hemma never writes the tunnel's compose labels; 'hemma doctor' reports a mismatch either way.").
			Affirmative("public").Negative("local only").
			Value(&public),
		huh.NewConfirm().Title("disabled").
			Description("Stop generating config for this service. It stays in services.yaml; its generated files are removed.").
			Affirmative("disabled").Negative("enabled").
			Value(&disabled),
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
			huh.NewText().Title("auth bypass paths").
				Description("One path per line, served without passing the gate (e.g. /health). Forward mode only.").
				Value(&bypass),
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
	// sync report (same contract as the create-user prompts).
	if err := huh.NewForm(groups...).WithOutput(os.Stderr).Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "Aborted — no changes made.")
			return 0
		}
		errf("%v", err)
		return 1
	}

	updated := svc
	updated.FQDN = strings.TrimSpace(fqdn)
	updated.Host = host
	updated.Backend = strings.TrimSpace(backend)
	updated.Public = public
	updated.Disabled = disabled
	if !isAuthService {
		if mode == "none" {
			// Selecting none clears the groups too — the only combination the
			// mutation layer would accept (groups require an auth gate).
			updated.Auth.Mode = config.AuthNone
			updated.Auth.Groups = nil
		} else {
			updated.Auth.Mode = config.AuthMode(mode)
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
			updated.Auth.Groups = gs
			updated.Auth.BypassPaths = splitLines(bypass)
		}
	}

	lines := summarizeServiceChanges(svc, updated)
	if len(lines) == 0 {
		fmt.Println("No changes.")
		return 0
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return persistUpdatedService(repoRoot, cfg, name, updated)
}

// persistUpdatedService is the shared tail of `update service`: both the
// flags form (cmdUpdate) and the interactive editor funnel through it —
// validate the resulting entry, persist, and run the right sync path
// (design §6.1). Zero mutation logic lives in the editor.
//
// Sync mode is chosen by the shape of the change, exactly as it is for the
// mutation verbs. Almost every edit is Incremental (it cannot orphan a file),
// but flipping `disabled` on CAN: a disabled service generates nothing, so its
// existing files must be deleted, and Incremental never deletes. Leaving them
// behind would be drift, which makes `apply` refuse. So a newly-disabled
// service goes through the targeted RemoveService primitive — the same one
// `hemma disable service` uses — rather than a plain reconcile.
func persistUpdatedService(repoRoot string, cfg *config.Config, name string, svc config.Service) int {
	// Groups only make sense with an auth gate — refuse the resulting combo
	// before persisting (validate-before-persist), whichever flag caused it.
	if svc.Auth.Mode == config.AuthNone && len(svc.Auth.Groups) > 0 {
		errf("Auth groups without an auth mode — pass --auth-mode forward|oidc, or clear the groups with --auth-groups ''.")
		return 2
	}
	wasDisabled := cfg.Services[name].Disabled
	cfg.Services[name] = svc
	if err := cfg.Save(); err != nil {
		errf("%v", err)
		return 1
	}
	fmt.Printf(tick+" Updated service %q\n", name)

	if svc.Disabled && !wasDisabled {
		mf := loadManifest(repoRoot, cfg)
		eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: mf}
		res, err := eng.RemoveService(name)
		if err != nil {
			errf("%v", err)
			return 1
		}
		fmt.Printf("Disabled — removed %d generated %s.\n", len(res.Deleted), plural(len(res.Deleted), "file"))
		return 0
	}
	return runSync(repoRoot, cfg, syncpkg.Incremental)
}

// groupOption is one entry of the interactive auth-groups multi-select:
// Name is the group name that would be persisted; Label is what the picker
// shows (name plus membership).
type groupOption struct {
	Name  string
	Label string
}

// buildGroupOptions assembles the auth-groups options from reality rather
// than free text: the union of (a) groups actually carried by users in the
// provider's users database and (b) groups referenced by services'
// auth.groups in services.yaml (including the service being edited, so its
// current groups are always present to pre-check). When users are known,
// labels list the members — "admins (soren)" — and a group referenced by
// services but carried by no user gets "(no members!)". usersKnown=false
// (users db missing/unreadable) falls back to (b) only, with plain labels,
// since membership can't be judged. Sorted by group name; pure — no I/O, no
// TTY — so it is testable without the huh form.
func buildGroupOptions(userGroups map[string][]string, usersKnown bool, services map[string]config.Service) []groupOption {
	members := map[string][]string{} // group -> usernames
	names := map[string]bool{}
	if usersKnown {
		for user, gs := range userGroups {
			for _, g := range gs {
				members[g] = append(members[g], user)
				names[g] = true
			}
		}
	}
	for _, s := range services {
		if s.Auth.Mode == config.AuthNone {
			continue
		}
		for _, g := range s.Auth.Groups {
			names[g] = true
		}
	}

	out := make([]groupOption, 0, len(names))
	for _, g := range sortedKeysOf(names) {
		label := g
		if usersKnown {
			if m := members[g]; len(m) > 0 {
				sort.Strings(m)
				label = fmt.Sprintf("%s (%s)", g, strings.Join(m, ", "))
			} else {
				label = g + " (no members!)"
			}
		}
		out = append(out, groupOption{Name: g, Label: label})
	}
	return out
}

// summarizeServiceChanges renders the "field: old → new" lines the editor
// prints before persisting. Empty means nothing changed (the caller exits 0
// without touching anything). Groups compare as sets (order-insensitive).
func summarizeServiceChanges(oldSvc, newSvc config.Service) []string {
	displayMode := func(m config.AuthMode) string {
		if m == config.AuthNone {
			return "none"
		}
		return string(m)
	}
	displayBool := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	displayPublic := func(p bool) string {
		if p {
			return "yes"
		}
		return "local only"
	}
	displayGroups := func(gs []string) string {
		if len(gs) == 0 {
			return "(none)"
		}
		gs = append([]string(nil), gs...)
		sort.Strings(gs)
		return strings.Join(gs, ", ")
	}

	var lines []string
	add := func(field, o, n string) {
		if o != n {
			lines = append(lines, fmt.Sprintf("  %s: %s → %s", field, o, n))
		}
	}
	add("fqdn", oldSvc.FQDN, newSvc.FQDN)
	add("host", oldSvc.Host, newSvc.Host)
	add("backend", oldSvc.Backend, newSvc.Backend)
	add("public", displayPublic(oldSvc.Public), displayPublic(newSvc.Public))
	add("disabled", displayBool(oldSvc.Disabled), displayBool(newSvc.Disabled))
	add("auth mode", displayMode(oldSvc.Auth.Mode), displayMode(newSvc.Auth.Mode))
	add("auth groups", displayGroups(oldSvc.Auth.Groups), displayGroups(newSvc.Auth.Groups))
	add("auth bypass paths", displayGroups(oldSvc.Auth.BypassPaths), displayGroups(newSvc.Auth.BypassPaths))
	return lines
}

// splitLines turns the bypass-paths text area into a trimmed, de-duplicated,
// order-preserving list. Order is preserved because Authelia rules are
// first-match, so the sequence the user typed is meaningful (unlike groups,
// which are OR'd and therefore sorted).
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}
