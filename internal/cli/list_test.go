package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written. The list command prints its table to stdout (not stderr),
// so tests must capture stdout to assert on the rendered table.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = orig }()
	fn()
	w.Close()
	return <-done
}

// listSetup builds a repo with one auth service and one non-auth service and
// returns the temp dir. The auth-snippet is set unless snippet is "".
func listSetup(t *testing.T, snippet string) string {
	t.Helper()
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if snippet != "" {
		if err := os.WriteFile(filepath.Join(dir, snippet),
			[]byte("forward_auth https://auth.example.com { }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"-C", dir, "defaults", "set", "auth-snippet", snippet}); code != 0 {
			t.Fatalf("set auth-snippet exit %d", code)
		}
	}
	if code := Run([]string{"-C", dir, "add", "service", "docs",
		"--fqdn", "docs.example.com", "--host", "appbox", "--backend", "paperless:8000", "--auth"}); code != 0 {
		t.Fatalf("add auth service exit %d", code)
	}
	if code := Run([]string{"-C", dir, "add", "service", "blog",
		"--fqdn", "blog.example.com", "--host", "appbox", "--backend", "ghost:2368"}); code != 0 {
		t.Fatalf("add plain service exit %d", code)
	}
	return dir
}

// serviceLine returns the rendered table row containing fqdn.
func serviceLine(out, fqdn string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, fqdn) && !strings.HasPrefix(strings.TrimSpace(ln), "(") {
			return ln
		}
	}
	return ""
}

// containsField reports whether one of line's whitespace-delimited fields is
// exactly want — a plain substring check would false-positive on something
// like "-" matching inside "paperless:8000" or "oidc" matching a longer word.
func containsField(line, want string) bool {
	for _, f := range strings.Fields(line) {
		if f == want {
			return true
		}
	}
	return false
}

// PUBLIC directly echoes the declared `public` field — no compose read, no
// unknown state, since hemma is now the sole author of the tunnel's ingress
// config rather than an observer of a foreign hand-maintained one.
func TestList_TunnelColumn(t *testing.T) {
	dir := listSetup(t, "")
	if code := Run([]string{"-C", dir, "defaults", "set", "tunnel-dir", "cloudflared"}); code != 0 {
		t.Fatalf("set tunnel-dir exit %d", code)
	}
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--public"}); code != 0 {
		t.Fatalf("--public exit %d", code)
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !strings.Contains(out, "TUNNEL") {
		t.Fatalf("expected TUNNEL header column, got:\n%s", out)
	}
	docsLine := serviceLine(out, "docs.example.com")
	blogLine := serviceLine(out, "blog.example.com")
	if docsLine == "" || blogLine == "" {
		t.Fatalf("missing service rows in:\n%s", out)
	}
	if !strings.Contains(docsLine, "yes") {
		t.Errorf("docs is public: true — row should show yes: %q", docsLine)
	}
	if !strings.Contains(blogLine, "no") {
		t.Errorf("blog is not public — row should show no: %q", blogLine)
	}
}

// --json prints every service as a JSON array of {name, fqdn, host, backend}
// objects, suppressing every other section (hosts, domains, auth,
// advisories) — intended for scripts, not human reading.
func TestList_JSON(t *testing.T) {
	dir := listSetup(t, "")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--json"}) })

	var got []struct {
		Name, FQDN, Host, Backend string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(got), got)
	}

	byName := map[string]struct{ FQDN, Host, Backend string }{}
	for _, s := range got {
		byName[s.Name] = struct{ FQDN, Host, Backend string }{s.FQDN, s.Host, s.Backend}
	}
	if docs := byName["docs"]; docs.FQDN != "docs.example.com" || docs.Host != "appbox" || docs.Backend != "paperless:8000" {
		t.Errorf("docs entry wrong: %+v", docs)
	}
	if blog := byName["blog"]; blog.FQDN != "blog.example.com" || blog.Host != "appbox" || blog.Backend != "ghost:2368" {
		t.Errorf("blog entry wrong: %+v", blog)
	}
}

// The AUTH column shows the mode (forward) for an auth service and - for a
// non-auth one, and the hint appears because at least one service uses auth.
func TestList_AuthColumn(t *testing.T) {
	dir := listSetup(t, "snip.caddy")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })

	// Header carries the AUTH column.
	if !strings.Contains(out, "AUTH") {
		t.Errorf("expected AUTH header column, got:\n%s", out)
	}
	// Find the two service rows and assert their trailing auth mode.
	var docsLine, blogLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "docs.example.com") {
			docsLine = ln
		}
		if strings.Contains(ln, "blog.example.com") {
			blogLine = ln
		}
	}
	if docsLine == "" || blogLine == "" {
		t.Fatalf("missing service rows in:\n%s", out)
	}
	// AUTH now sits before the always-present PUBLIC column, so check the
	// field's value directly rather than assume it's the row's suffix.
	if !containsField(docsLine, "forward") {
		t.Errorf("auth service row should show forward, got %q", docsLine)
	}
	if !containsField(blogLine, "-") {
		t.Errorf("non-auth service row should show -, got %q", blogLine)
	}
	if !strings.Contains(out, "imports the (auth) snippet") {
		t.Errorf("expected the auth hint when a service uses auth, got:\n%s", out)
	}
}

// oidc renders a PLAIN reverse_proxy (no import auth) and the list AUTH column
// shows "oidc".
func TestList_OIDCColumn(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	if code := Run([]string{"-C", dir, "add", "service", "app",
		"--fqdn", "app.example.com", "--host", "appbox", "--backend", "app:3000", "--auth-mode", "oidc"}); code != 0 {
		t.Fatalf("add oidc service exit %d", code)
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	var line string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "app.example.com") {
			line = ln
		}
	}
	// AUTH now sits before the always-present PUBLIC column.
	if !containsField(line, "oidc") {
		t.Errorf("oidc service row should show oidc, got %q", line)
	}
	// The generated Caddy site must be a plain reverse_proxy (no import auth).
	b, err := os.ReadFile(filepath.Join(dir, "appbox", "caddy", "data", "sites", "app.caddy"))
	if err != nil {
		t.Fatalf("read site: %v", err)
	}
	if strings.Contains(string(b), "import auth") {
		t.Errorf("oidc site must not import auth:\n%s", b)
	}
}

// The completion command prints a script for bash/zsh and rejects others.
func TestRun_Completion(t *testing.T) {
	bash := captureStdout(t, func() {
		if code := Run([]string{"completion", "bash"}); code != 0 {
			t.Fatalf("completion bash exit %d", code)
		}
	})
	if !strings.Contains(bash, "complete -F _hemma hemma") {
		t.Errorf("bash script missing complete registration:\n%s", bash)
	}
	zsh := captureStdout(t, func() {
		if code := Run([]string{"completion", "zsh"}); code != 0 {
			t.Fatalf("completion zsh exit %d", code)
		}
	})
	if !strings.Contains(zsh, "#compdef hemma") {
		t.Errorf("zsh script missing #compdef header:\n%s", zsh)
	}
	if code := Run([]string{"completion", "fish"}); code != 2 {
		t.Errorf("unsupported shell should exit 2, got %d", code)
	}
	if code := Run([]string{"completion"}); code != 2 {
		t.Errorf("missing shell arg should exit 2, got %d", code)
	}
}

// The Auth section shows the snippet when set, and is hidden entirely when no
// auth is configured.
func TestList_AuthSection(t *testing.T) {
	// Set: the Auth section appears with the snippet path.
	dir := listSetup(t, "snip.caddy")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if !strings.Contains(out, "== Auth ==") {
		t.Errorf("expected an '== Auth ==' section, got:\n%s", out)
	}
	if !strings.Contains(out, "snippet:  snip.caddy") {
		t.Errorf("expected 'snippet:  snip.caddy', got:\n%s", out)
	}

	// Unset: no auth-snippet, single non-auth service → no Auth section at all.
	dir2 := t.TempDir()
	mkdirs(t, dir2, "resolver", "appbox")
	seed(t, dir2)
	if code := Run([]string{"-C", dir2, "add", "service", "blog",
		"--fqdn", "blog.example.com", "--host", "appbox", "--backend", "ghost:2368"}); code != 0 {
		t.Fatalf("add plain service exit %d", code)
	}
	out2 := captureStdout(t, func() { Run([]string{"-C", dir2, "list", "--all"}) })
	if strings.Contains(out2, "== Auth ==") {
		t.Errorf("Auth section should be hidden when nothing is configured, got:\n%s", out2)
	}
	// With no auth service, the disable hint must NOT appear.
	if strings.Contains(out2, "imports the auth snippet") {
		t.Errorf("disable hint should be absent when no service uses auth, got:\n%s", out2)
	}
}

// The auth_service is the one row where "-" actively misinforms: it is the
// login portal, so it would read as unprotected while being the thing doing the
// protecting. No auth MODE fits it, so the ROLE is shown instead — parenthesised
// like the (dns_host) marker in the Hosts section.
func TestList_AuthServiceShowsPortal(t *testing.T) {
	dir := listSetup(t, "snip.caddy")
	// docs is created with --auth by listSetup; clear it so the auth_service is
	// in its normal state (a mode on the portal is refused by the planner).
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--auth-mode", "none"}); code != 0 {
		t.Fatalf("clear auth exit %d", code)
	}
	if code := Run([]string{"-C", dir, "defaults", "set", "auth-service", "docs"}); code != 0 {
		t.Fatalf("set auth-service exit %d", code)
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })

	docs := serviceLine(out, "docs.example.com")
	if !strings.Contains(docs, "(portal)") {
		t.Errorf("auth_service row should show (portal), got %q", docs)
	}
	// It must not still read as unprotected.
	if f := strings.Fields(docs); len(f) > 0 && f[len(f)-1] == "-" {
		t.Errorf("auth_service row must not end in \"-\", got %q", docs)
	}
	// A non-portal service is unaffected.
	if blog := serviceLine(out, "blog.example.com"); strings.Contains(blog, "portal") {
		t.Errorf("only the auth_service gets the marker, got %q", blog)
	}
	if !strings.Contains(out, "(portal) = this service IS the auth provider") {
		t.Errorf("legend should explain the marker, got:\n%s", out)
	}
}

// A mode set on the auth_service anyway (odd, but representable — the planner
// refuses it at sync time, not on persist) must not be hidden by the marker.
func TestList_AuthServiceKeepsExplicitMode(t *testing.T) {
	dir := listSetup(t, "")
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	svc := cfg.Services["blog"]
	svc.Auth.Mode = config.AuthOIDC
	cfg.Services["blog"] = svc
	cfg.Defaults.AuthService = "blog"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	line := serviceLine(out, "blog.example.com")
	if !strings.Contains(line, "oidc") || !strings.Contains(line, "(portal)") {
		t.Errorf("declared mode must survive alongside the marker, got %q", line)
	}
}

// listHorizonSetup extends listSetup: docs gets {local: bypass, public:
// two_factor}, a third service is oidc (mode without a policy), and blog stays
// ungated. Nothing here is on the tunnel, so it also pins that PUBLIC is shown
// for a tunnel-less service.
func listHorizonSetup(t *testing.T) string {
	t.Helper()
	dir := listSetup(t, "snip.caddy")
	if code := Run([]string{"-C", dir, "update", "service", "docs",
		"--auth-policy-local", "bypass", "--auth-policy-public", "two_factor"}); code != 0 {
		t.Fatalf("set policies exit %d", code)
	}
	if code := Run([]string{"-C", dir, "add", "service", "app",
		"--fqdn", "app.example.com", "--host", "appbox", "--backend", "app:3000", "--auth-mode", "oidc"}); code != 0 {
		t.Fatalf("add oidc service exit %d", code)
	}
	return dir
}

// Without --policy the table has no LOCAL/PUBLIC columns; with it, forward
// services show their effective levels and every other mode shows "-".
func TestList_PolicyColumns(t *testing.T) {
	dir := listHorizonSetup(t)

	plain := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if strings.Contains(plain, "PUBLIC") || strings.Contains(plain, "LOCAL") {
		t.Errorf("horizon columns must be opt-in, got:\n%s", plain)
	}

	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all", "--policy"}) })
	if !containsField(out, "LOCAL") || !containsField(out, "PUBLIC") {
		t.Fatalf("expected LOCAL and PUBLIC headers, got:\n%s", out)
	}
	docs := serviceLine(out, "docs.example.com")
	if !containsField(docs, "bypass") || !containsField(docs, "two_factor") {
		t.Errorf("docs row should show bypass + two_factor, got %q", docs)
	}
	if blog := serviceLine(out, "blog.example.com"); strings.Contains(blog, "one_factor") || strings.Contains(blog, "bypass") {
		t.Errorf("ungated row must carry no policy, got %q", blog)
	}
	if app := serviceLine(out, "app.example.com"); strings.Contains(app, "one_factor") {
		t.Errorf("oidc carries no policy, got %q", app)
	}
}

// An unset horizon resolves to the default (one_factor), not "-".
func TestList_PolicyDefaultsToOneFactor(t *testing.T) {
	dir := listSetup(t, "snip.caddy")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all", "--policy"}) })
	docs := strings.Fields(serviceLine(out, "docs.example.com"))
	n := 0
	for _, f := range docs {
		if f == "one_factor" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("bare forward service should show one_factor twice, got %q", docs)
	}
}

// --json always carries auth_mode and tunnel; the policy fields appear only
// for forward mode.
func TestList_JSONPolicy(t *testing.T) {
	dir := listHorizonSetup(t)
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--json"}) })
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	by := map[string]map[string]any{}
	for _, s := range got {
		by[s["name"].(string)] = s
	}
	docs := by["docs"]
	if docs["auth_mode"] != "forward" || docs["policy_local"] != "bypass" || docs["policy_public"] != "two_factor" || docs["tunnel"] != false {
		t.Errorf("docs JSON wrong: %v", docs)
	}
	if by["blog"]["auth_mode"] != "none" {
		t.Errorf("ungated service should report auth_mode none, got %v", by["blog"]["auth_mode"])
	}
	for _, name := range []string{"blog", "app"} {
		if _, ok := by[name]["policy_local"]; ok {
			t.Errorf("%s has no policy; policy_local must be omitted: %v", name, by[name])
		}
		if _, ok := by[name]["auth_mode"]; !ok {
			t.Errorf("%s must still carry auth_mode: %v", name, by[name])
		}
	}
}
