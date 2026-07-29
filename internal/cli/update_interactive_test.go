package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hemma/internal/config"
)

// Dispatch: `update service <name>` with zero flags is the interactive
// editor, TTY-gated. Under `go test` stdin is not a terminal, so it must
// refuse with exit 2 and point at the flags — never hang on a hidden prompt.
func TestRun_UpdateZeroFlagsNonTTYRefused(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})

	var code int
	out := captureStderr(t, func() {
		code = Run([]string{"-C", dir, "update", "service", "docs"})
	})
	if code != 2 {
		t.Errorf("zero-flag update on non-TTY stdin should exit 2, got %d", code)
	}
	if !strings.Contains(out, "stdin is not a terminal") {
		t.Errorf("refusal should explain the TTY gate, got: %q", out)
	}
}

// Dispatch: any flag present keeps the existing non-interactive path,
// unchanged (no TTY needed).
func TestRun_UpdateWithFlagsStaysNonInteractive(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"})

	if code := Run([]string{"-C", dir, "update", "service", "docs", "--backend", "paperless:8001"}); code != 0 {
		t.Fatalf("flag update should exit 0 without a TTY, got %d", code)
	}
	cfg, err := config.Load(dir + "/" + configName)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services["docs"].Backend; got != "paperless:8001" {
		t.Errorf("backend not updated via flags path: %q", got)
	}
}

func svcMap(entries map[string]config.Auth) map[string]config.Service {
	m := map[string]config.Service{}
	for name, a := range entries {
		m[name] = config.Service{FQDN: name + ".example.com", Host: "appbox", Backend: name + ":80", Auth: a}
	}
	return m
}

// Group options are the union of groups on real users (a) and groups
// referenced by services' auth.groups (b), sorted, with members in the label.
func TestBuildGroupOptions_UnionAndMemberLabels(t *testing.T) {
	users := map[string][]string{
		"soren": {"admins", "family"},
		"maria": {"family"},
		"guest": {"visitors"}, // on a user only — still offered
	}
	services := svcMap(map[string]config.Auth{
		"docs":  {Mode: config.AuthForward, Groups: []string{"admins"}},
		"media": {Mode: config.AuthOIDC, Groups: []string{"family", "streamers"}}, // streamers: service-only
		"open":  {Mode: config.AuthNone},                                          // no gate, contributes nothing
	})

	got := buildGroupOptions(users, true, services)
	want := []groupOption{
		{Name: "admins", Label: "admins (soren)"},
		{Name: "family", Label: "family (maria, soren)"},
		{Name: "streamers", Label: "streamers (no members!)"},
		{Name: "visitors", Label: "visitors (guest)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options mismatch:\n got  %v\n want %v", got, want)
	}
}

// A group referenced by services but carried by no user is flagged loudly —
// nobody could pass its access rule.
func TestBuildGroupOptions_NoMembersLabel(t *testing.T) {
	services := svcMap(map[string]config.Auth{
		"docs": {Mode: config.AuthForward, Groups: []string{"ghosts"}},
	})
	got := buildGroupOptions(map[string][]string{}, true, services)
	want := []groupOption{{Name: "ghosts", Label: "ghosts (no members!)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// With the users database missing/unreadable (usersKnown=false), only the
// services side is offered, with plain labels — membership can't be judged,
// so no "(no members!)" false alarms.
func TestBuildGroupOptions_MissingUsersDBFallsBackToServices(t *testing.T) {
	users := map[string][]string{"soren": {"admins"}} // must be ignored
	services := svcMap(map[string]config.Auth{
		"docs":  {Mode: config.AuthForward, Groups: []string{"family"}},
		"media": {Mode: config.AuthOIDC, Groups: []string{"admins"}},
	})
	got := buildGroupOptions(users, false, services)
	want := []groupOption{
		{Name: "admins", Label: "admins"},
		{Name: "family", Label: "family"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The submit summary lists exactly the changed fields, old → new; identical
// services produce no lines (the editor exits 0 touching nothing).
func TestSummarizeServiceChanges(t *testing.T) {
	oldSvc := config.Service{FQDN: "a.example.com", Host: "appbox", Backend: "a:80",
		Auth: config.Auth{Mode: config.AuthForward, Groups: []string{"family", "admins"}}}

	if lines := summarizeServiceChanges(oldSvc, oldSvc); len(lines) != 0 {
		t.Errorf("identical services should summarize to nothing, got %v", lines)
	}
	// Same groups in a different order is not a change (set comparison).
	reordered := oldSvc
	reordered.Auth.Groups = []string{"admins", "family"}
	if lines := summarizeServiceChanges(oldSvc, reordered); len(lines) != 0 {
		t.Errorf("group order should not count as a change, got %v", lines)
	}

	newSvc := oldSvc
	newSvc.FQDN = "b.example.com"
	newSvc.Auth.Mode = config.AuthNone
	newSvc.Auth.Groups = nil
	lines := summarizeServiceChanges(oldSvc, newSvc)
	want := []string{
		"  fqdn: a.example.com → b.example.com",
		"  auth mode: forward → none",
		"  auth groups: admins, family → (none)",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("summary mismatch:\n got  %v\n want %v", lines, want)
	}
}

// Flipping `disabled` through the shared update tail must DELETE the generated
// files, not merely stop regenerating them. persistUpdatedService syncs
// Incremental, which never deletes — so without the RemoveService branch the
// files would be orphaned, the repo would show drift, and `apply` would refuse.
// This is the trap that makes `disabled` different from every other field the
// interactive editor can now set.
func TestPersistUpdatedService_DisablingRemovesGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	site := filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy")
	rec := filepath.Join(dir, "resolver", "pihole", "data", "dnsmasq.d", "docs.generated.conf")
	for _, p := range []string{site, rec} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s after add: %v", p, err)
		}
	}

	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	svc := cfg.Services["docs"]
	svc.Disabled = true
	captureStdout(t, func() {
		if code := persistUpdatedService(dir, cfg, "docs", svc); code != 0 {
			t.Errorf("persistUpdatedService exit %d", code)
		}
	})

	for _, p := range []string{site, rec} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted when the service is disabled (err=%v)", p, err)
		}
	}
	// The repo must be clean afterwards — orphaned files here would make
	// `hemma apply` refuse to run.
	out := captureStdout(t, func() { Run([]string{"-C", dir, "doctor"}) })
	if strings.Contains(out, "out of sync") {
		t.Errorf("disabling must not leave drift:\n%s", out)
	}
}

// Re-enabling regenerates: the ordinary Incremental path handles it, so the
// round trip must land back where it started.
func TestPersistUpdatedService_ReEnablingRegenerates(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	site := filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy")
	before, err := os.ReadFile(site)
	if err != nil {
		t.Fatal(err)
	}

	set := func(disabled bool) {
		cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		svc := cfg.Services["docs"]
		svc.Disabled = disabled
		captureStdout(t, func() { persistUpdatedService(dir, cfg, "docs", svc) })
	}
	set(true)
	set(false)

	after, err := os.ReadFile(site)
	if err != nil {
		t.Fatalf("re-enabling should regenerate the site file: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("regenerated content differs:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A plain field edit still takes the Incremental path and leaves the other
// generated files alone — the RemoveService branch must be narrow.
func TestPersistUpdatedService_OrdinaryEditKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000"}); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.TunnelDir = "cloudflared" // required for any public: true service to plan
	svc := cfg.Services["docs"]
	svc.Public = true // the kind of edit the editor now allows
	captureStdout(t, func() {
		if code := persistUpdatedService(dir, cfg, "docs", svc); code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	if _, err := os.Stat(filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy")); err != nil {
		t.Errorf("ordinary edit must not delete generated files: %v", err)
	}
}
