package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
)

// printGroupsSection appends the auth-groups picture to `list`: the union of
// groups from the provider's users database (user -> groups) and
// services.yaml (service -> auth.groups), one block per group, showing the
// users in it and the services restricted to it. Groups present on only one
// side still list — those are exactly the interesting cases (doctor warns on
// the services-without-users one). Read-only; usernames only, never password
// hashes or emails. When the users database is missing or unreadable, a
// services-only view is shown with a note. Hidden entirely when no group
// exists on either side (keeps a no-auth repo's output clean, like == Auth ==).
func printGroupsSection(repoRoot string, cfg *config.Config) {
	users, usersNote := loadUserGroups(repoRoot, cfg)

	// group -> usernames.
	groupUsers := map[string][]string{}
	for u, groups := range users {
		for _, g := range groups {
			groupUsers[g] = append(groupUsers[g], u)
		}
	}
	// group -> "service (mode)" labels.
	groupServices := map[string][]string{}
	for name, s := range cfg.Services {
		if s.Auth.Mode == config.AuthNone {
			continue
		}
		for _, g := range s.Auth.Groups {
			groupServices[g] = append(groupServices[g], fmt.Sprintf("%s (%s)", name, s.Auth.Mode))
		}
	}

	// Union of both sources, sorted.
	names := map[string]bool{}
	for g := range groupUsers {
		names[g] = true
	}
	for g := range groupServices {
		names[g] = true
	}
	if len(names) == 0 {
		return
	}
	groups := sortedKeysOf(names)

	fmt.Printf("\n%s== Groups (%d) ==%s\n", boldOn, len(groups), boldOff)
	if usersNote != "" {
		fmt.Printf("  %s users unknown — %s (services-only view)\n", warn, usersNote)
	}
	for _, g := range groups {
		fmt.Printf("  %s%s%s\n", boldOn, g, boldOff)
		fmt.Printf("    users:     %s\n", joinOrPlaceholder(groupUsers[g], usersNote != "", "no user has this group"))
		fmt.Printf("    services:  %s\n", joinOrPlaceholder(groupServices[g], false, "no service restricts to this group"))
	}
}

// joinOrPlaceholder renders a sorted comma list, or a placeholder when empty.
// unknown suppresses the empty-side explanation (the users db was unreadable,
// so absence means nothing).
func joinOrPlaceholder(items []string, unknown bool, emptyReason string) string {
	if len(items) == 0 {
		if unknown {
			return "(unknown)"
		}
		return "(none — " + emptyReason + ")"
	}
	sort.Strings(items)
	return strings.Join(items, ", ")
}

// loadUserGroups reads username -> groups from the auth provider's users
// database, locating it via the same auth_service/host convention as the
// doctor checks. A non-empty note (and nil map) means the users side is
// unavailable; the note says why.
func loadUserGroups(repoRoot string, cfg *config.Config) (map[string][]string, string) {
	if cfg.Defaults.AuthService == "" {
		return nil, "auth_service not set (hemma defaults set auth-service <name>)"
	}
	authSvc, ok := cfg.Services[cfg.Defaults.AuthService]
	if !ok {
		return nil, fmt.Sprintf("auth_service %q is not a defined service", cfg.Defaults.AuthService)
	}
	hostM, ok := cfg.Hosts[authSvc.Host]
	if !ok {
		return nil, fmt.Sprintf("auth_service host %q is not defined", authSvc.Host)
	}
	provider := auth.Default()
	providerCfg := filepath.Join(repoRoot, hostM.ResolvedDir(authSvc.Host), provider.ConfigPath())
	users, err := provider.UserGroups(providerCfg)
	if err != nil {
		return nil, err.Error()
	}
	if users == nil {
		return nil, "users database not found next to the provider config"
	}
	return users, ""
}
