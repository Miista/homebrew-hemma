package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUsersFixture creates a config dir with configuration.yml (optionally
// declaring authentication_backend.file.path) and a users db under name.
func writeUsersFixture(t *testing.T, cfgYAML, usersName, usersYAML string) (cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "configuration.yml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if usersYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, usersName), []byte(usersYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath
}

const usersDB = `users:
  alice:
    disabled: false
    displayname: alice
    email: alice@example.com
    password: '$argon2id$...'
    groups:
      - admins
      - media
  bob:
    disabled: false
    displayname: bob
    email: bob@example.com
    password: '$argon2id$...'
    groups: []
`

func TestValidateUsers_GroupTypoAndUnreachableService(t *testing.T) {
	cfgPath := writeUsersFixture(t, "authentication_backend:\n  file:\n    path: /config/users_database.yml\n", "users_database.yml", usersDB)
	svcs := []Service{
		{Name: "jellyfin", FQDN: "jf.example.com", Mode: ModeForward, Groups: []string{"media"}},   // ok
		{Name: "grafana", FQDN: "gf.example.com", Mode: ModeOIDC, Groups: []string{"adminz"}},      // typo
		{Name: "paperless", FQDN: "pl.example.com", Mode: ModeForward, Groups: []string{"admins", "editors"}}, // editors unknown, admins populated
	}
	w := (authelia{}).ValidateUsers(cfgPath, svcs)
	joined := strings.Join(w, "\n")
	// grafana: adminz typo + nobody-can-access; paperless: editors typo only
	// (admins is populated); jellyfin: clean.
	if len(w) != 3 {
		t.Fatalf("want 3 warnings, got %d:\n%s", len(w), joined)
	}
	if !strings.Contains(joined, `"adminz" (service grafana)`) {
		t.Errorf("missing typo warning for grafana/adminz:\n%s", joined)
	}
	if !strings.Contains(joined, "nobody can access") || !strings.Contains(joined, "grafana's allowed groups") {
		t.Errorf("missing nobody-can-access warning for grafana:\n%s", joined)
	}
	if !strings.Contains(joined, `"editors" (service paperless)`) {
		t.Errorf("missing typo warning for paperless/editors:\n%s", joined)
	}
	// No hashes or emails may leak into warnings.
	if strings.Contains(joined, "argon2") || strings.Contains(joined, "@") {
		t.Errorf("warnings must not contain hashes or emails:\n%s", joined)
	}
}

func TestValidateUsers_NobodyCanAccess(t *testing.T) {
	db := `users:
  alice:
    groups:
      - admins
`
	cfgPath := writeUsersFixture(t, "", "users_database.yml", db)
	svcs := []Service{
		// alice is in admins — reachable, no warnings.
		{Name: "wiki", FQDN: "w.example.com", Mode: ModeForward, Groups: []string{"admins"}},
		// no user in ops or oncall — per-group typo warnings AND the
		// nobody-can-access summary.
		{Name: "pager", FQDN: "p.example.com", Mode: ModeOIDC, Groups: []string{"ops", "oncall"}},
	}
	w := (authelia{}).ValidateUsers(cfgPath, svcs)
	joined := strings.Join(w, "\n")
	if len(w) != 3 {
		t.Fatalf("want 3 warnings (2 typo + 1 unreachable), got %d:\n%s", len(w), joined)
	}
	if !strings.Contains(joined, "no user is in any of service pager's allowed groups (ops, oncall) — nobody can access it.") {
		t.Errorf("missing nobody-can-access warning:\n%s", joined)
	}
	if strings.Contains(joined, "wiki") {
		t.Errorf("reachable service must not be warned about:\n%s", joined)
	}
}

func TestValidateUsers_MissingDBIsSilent(t *testing.T) {
	cfgPath := writeUsersFixture(t, "", "", "")
	svcs := []Service{{Name: "x", FQDN: "x.example.com", Mode: ModeForward, Groups: []string{"g"}}}
	if w := (authelia{}).ValidateUsers(cfgPath, svcs); w != nil {
		t.Errorf("missing users db must be silent (gated check), got %v", w)
	}
}

func TestValidateUsers_NoGroupsNothingToCheck(t *testing.T) {
	cfgPath := writeUsersFixture(t, "", "users_database.yml", usersDB)
	svcs := []Service{{Name: "x", FQDN: "x.example.com", Mode: ModeForward}}
	if w := (authelia{}).ValidateUsers(cfgPath, svcs); w != nil {
		t.Errorf("no groups referenced -> no warnings, got %v", w)
	}
}

func TestValidateUsers_CustomDBNameFromConfig(t *testing.T) {
	// authentication_backend.file.path with a non-default basename: the
	// basename is honored, resolved next to configuration.yml.
	cfgPath := writeUsersFixture(t, "authentication_backend:\n  file:\n    path: /config/users.custom.yml\n", "users.custom.yml", usersDB)
	svcs := []Service{{Name: "g", FQDN: "g.example.com", Mode: ModeOIDC, Groups: []string{"nosuch"}}}
	w := (authelia{}).ValidateUsers(cfgPath, svcs)
	if len(w) != 2 || !strings.Contains(w[0], "users.custom.yml") {
		t.Errorf("want typo warning naming users.custom.yml (+ unreachable warning), got %v", w)
	}
}

func TestValidateUsers_UnparseableDBSoftAdvisory(t *testing.T) {
	cfgPath := writeUsersFixture(t, "", "users_database.yml", "users: [not a map\n")
	svcs := []Service{{Name: "x", FQDN: "x.example.com", Mode: ModeForward, Groups: []string{"g"}}}
	w := (authelia{}).ValidateUsers(cfgPath, svcs)
	if len(w) != 1 || !strings.Contains(w[0], "could not cross-check") {
		t.Errorf("want one soft advisory, got %v", w)
	}
}
