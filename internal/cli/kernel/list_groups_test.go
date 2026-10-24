package kernel

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestList_GroupsSection_UnionOfUsersAndServices(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir) // grafana restricts to [admins]; docsapp oidc, no groups
	writeAutheliaFixture(t, dir, `users:
  alice:
    displayname: alice
    email: alice@example.com
    password: '$argon2id$secret'
    groups: [admins, media]
  bob:
    groups: [media]
`)
	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "list", "--all"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "== Groups (2) ==") {
		t.Errorf("want union of 2 groups (admins, media):\n%s", out)
	}
	// admins: user + service on both sides.
	if !strings.Contains(out, "users:     alice\n") || !strings.Contains(out, "services:  grafana (oidc)") {
		t.Errorf("admins should show alice and grafana (oidc):\n%s", out)
	}
	// media: users-only group still lists, with a services placeholder;
	// its users sorted on one line.
	if !strings.Contains(out, "no service restricts to this group") {
		t.Errorf("users-only group media should list with a services placeholder:\n%s", out)
	}
	if !strings.Contains(out, "alice, bob") {
		t.Errorf("media users should be sorted 'alice, bob':\n%s", out)
	}
	// Never leak hashes or emails.
	if strings.Contains(out, "argon2id") || strings.Contains(out, "@example.com") {
		t.Errorf("output must not contain hashes or emails:\n%s", out)
	}
}

func TestList_GroupsSection_ServiceOnlyGroupStillLists(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir) // grafana -> [admins]
	writeAutheliaFixture(t, dir, "users:\n  alice:\n    groups: []\n")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !strings.Contains(out, "admins") || !strings.Contains(out, "no user has this group") {
		t.Errorf("service-only group should list with a users placeholder:\n%s", out)
	}
}

func TestList_GroupsSection_MissingUsersDBFallsBackToServicesOnly(t *testing.T) {
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAutheliaFixture(t, dir, "") // provider config present, users db absent
	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "list", "--all"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (services-only view):\n%s", code, out)
	}
	if !strings.Contains(out, "users unknown") || !strings.Contains(out, "users database not found") {
		t.Errorf("missing users db should be noted:\n%s", out)
	}
	if !strings.Contains(out, "services:  grafana (oidc)") || !strings.Contains(out, "users:     (unknown)") {
		t.Errorf("services side should still list, users side (unknown):\n%s", out)
	}
}

func TestList_GroupsSection_HiddenWhenNoGroupsAnywhere(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir) // plain fixture: no auth, no groups
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if strings.Contains(out, "== Groups") {
		t.Errorf("Groups section should be hidden when no group exists anywhere:\n%s", out)
	}
}

func TestUserGroups_ProviderReadsFixture(t *testing.T) {
	// The provider-level read used by the Groups section honors the configured
	// users-db basename and never surfaces hashes.
	dir := t.TempDir()
	seedWithAuth(t, dir)
	writeAutheliaFixture(t, dir, "users:\n  alice:\n    groups: [admins]\n")
	cfg, _ := loadExisting(stdEnv(), filepath.Join(dir, configName), "test")
	users, note := loadUserGroups(dir, cfg)
	if note != "" {
		t.Fatalf("unexpected note %q", note)
	}
	if got := users["alice"]; len(got) != 1 || got[0] != "admins" {
		t.Errorf("alice groups = %v, want [admins]", got)
	}
}
