package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Read-only cross-validation of Authelia's users database against the
// services' auth groups. hemma never writes users_database.yml (it is
// hand-owned and secret-bearing); it only parses username -> groups to catch
// group typos and services nobody can access. Warnings never include password
// hashes or email addresses.

// autheliaUsersFile is the conventional users-database filename, expected in
// the same directory as configuration.yml.
const autheliaUsersFile = "users_database.yml"

// autheliaBackendDoc is the sliver of configuration.yml naming the file
// backend's users database.
type autheliaBackendDoc struct {
	AuthenticationBackend struct {
		File struct {
			Path string `yaml:"path"`
		} `yaml:"file"`
	} `yaml:"authentication_backend"`
}

// autheliaUsersDoc is the parsed shape of users_database.yml — only the
// group memberships; passwords/emails are deliberately not read into memory.
type autheliaUsersDoc struct {
	Users map[string]struct {
		Groups []string `yaml:"groups"`
	} `yaml:"users"`
}

// usersDatabasePath derives the users-database path from the provider config
// at cfgPath: if configuration.yml declares authentication_backend.file.path,
// its basename is used (the declared path is container-internal, so only the
// filename transfers to the host-side config dir); otherwise the conventional
// users_database.yml. Always in the same directory as configuration.yml.
func usersDatabasePath(cfgPath string) string {
	name := autheliaUsersFile
	if data, err := os.ReadFile(cfgPath); err == nil {
		var doc autheliaBackendDoc
		if yaml.Unmarshal(data, &doc) == nil && doc.AuthenticationBackend.File.Path != "" {
			name = filepath.Base(doc.AuthenticationBackend.File.Path)
		}
	}
	return filepath.Join(filepath.Dir(cfgPath), name)
}

// UserGroups reads the users database (located relative to the provider
// config at cfgPath) and returns username -> groups. A missing database
// returns (nil, nil) so callers can distinguish "no file backend" from a read
// or parse error. Passwords and emails are never read into memory.
func (authelia) UserGroups(cfgPath string) (map[string][]string, error) {
	usersPath := usersDatabasePath(cfgPath)
	data, err := os.ReadFile(usersPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc autheliaUsersDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(usersPath), err)
	}
	users := make(map[string][]string, len(doc.Users))
	for name, u := range doc.Users {
		users[name] = u.Groups
	}
	return users, nil
}

// ValidateUsers cross-checks the users database against the services' auth
// groups (advisory; report-but-proceed):
//   - every group a service references should exist on at least one user
//     (catches typos);
//   - every auth-gated service with groups should have at least one user in
//     an allowed group ("nobody can access X").
//
// Gated on the users database existing at the conventional path next to the
// provider config — absent file returns nil (LDAP backends etc. have no file
// to check). An unreadable/unparseable file yields a single soft advisory.
func (a authelia) ValidateUsers(cfgPath string, services []Service) []string {
	var withGroups []Service
	for _, s := range services {
		if len(s.Groups) > 0 {
			withGroups = append(withGroups, s)
		}
	}
	if len(withGroups) == 0 {
		return nil
	}
	sort.Slice(withGroups, func(i, j int) bool { return withGroups[i].Name < withGroups[j].Name })

	users, err := a.UserGroups(cfgPath)
	if err != nil {
		return []string{fmt.Sprintf("could not cross-check auth groups: %v", err)}
	}
	if users == nil {
		return nil // gate: no file backend here, nothing to cross-check
	}
	usersPath := usersDatabasePath(cfgPath)

	// group -> member count (usernames themselves are not needed in warnings).
	members := map[string]int{}
	for _, groups := range users {
		for _, g := range groups {
			members[g]++
		}
	}

	var w []string
	for _, s := range withGroups {
		anyMember := false
		for _, g := range s.Groups {
			if members[g] > 0 {
				anyMember = true
				continue
			}
			w = append(w, fmt.Sprintf("auth group %q (service %s) is not assigned to any user in %s — check for a typo: fix the list with 'hemma update service %s --auth-groups <groups>' or add the group to a user in that file.", g, s.Name, filepath.Base(usersPath), s.Name))
		}
		if !anyMember {
			w = append(w, fmt.Sprintf("no user is in any of service %s's allowed groups (%s) — nobody can access it. Add a group to a user in %s or relax the service's --auth-groups.", s.Name, strings.Join(s.Groups, ", "), filepath.Base(usersPath)))
		}
	}
	return w
}
