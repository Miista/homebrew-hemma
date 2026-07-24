package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// doctorSetup builds a repo with an appbox host, a domain, and the given
// services, returning the repo dir. Services are added via the CLI so the
// fixture goes through the same validation and sync path as real use.
func doctorSetup(t *testing.T) string {
	t.Helper()
	dir := listSetup(t, "")
	return dir
}

// setPublic sets (or clears) the declared `public` field on a service by
// editing services.yaml through the config package — there is no CLI flag.
func setPublic(t *testing.T, dir, svc string, want *bool) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Services[svc]
	s.Public = want
	cfg.Services[svc] = s
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// setAuthMode sets a service's auth mode via the CLI.
func setAuthMode(t *testing.T, dir, svc, mode string) {
	t.Helper()
	if code := Run([]string{"-C", dir, "update", "service", svc, "--auth-mode", mode}); code != 0 {
		t.Fatalf("update service %s --auth-mode %s exit %d", svc, mode, code)
	}
}

// doctorOut runs doctor and returns its combined output plus exit code.
func doctorOut(t *testing.T, dir string) (string, int) {
	t.Helper()
	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "doctor"}) })
	return out, code
}

func ptr(b bool) *bool { return &b }

// A forward-auth service served DIRECT from the tunnel is publicly reachable
// with the auth gate bypassed. This is the highest-severity check here, and it
// must count as a doctor problem.
func TestDoctorPublic_AuthBypassDirectIngress(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "publicly reachable WITHOUT auth") {
		t.Errorf("expected the auth-bypass advisory, got:\n%s", out)
	}
	if !strings.Contains(out, "cloudflare.io/reverseproxy") {
		t.Errorf("advisory must carry the proxy label to add, got:\n%s", out)
	}
	if code == 0 {
		t.Error("auth bypass must be a doctor problem (non-zero exit)")
	}
}

// Routed through a proxy, the same service is correctly gated — no advisory.
func TestDoctorPublic_AuthBypassSilentWhenProxied(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
      cloudflare.io/reverseproxy: "https://caddy:443"
`)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "WITHOUT auth") {
		t.Errorf("proxied forward-auth service must not warn, got:\n%s", out)
	}
}

// An oidc service authenticates in the app itself, so direct ingress is by
// design; a no-auth service has no gate to bypass. Neither may warn.
func TestDoctorPublic_AuthBypassOnlyAppliesToForward(t *testing.T) {
	for _, mode := range []string{"oidc", "none"} {
		dir := doctorSetup(t)
		setAuthMode(t, dir, "docs", mode)
		writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
		out, _ := doctorOut(t, dir)
		if strings.Contains(out, "WITHOUT auth") {
			t.Errorf("mode %s must not trigger the bypass check, got:\n%s", mode, out)
		}
	}
}

// The auth provider's own service is exempt: it IS the login portal, so it must
// be publicly reachable without passing its own gate.
func TestDoctorPublic_AuthBypassExemptsAuthService(t *testing.T) {
	dir := doctorSetup(t)
	// Set both directly: `set auth-service` on a forward-auth service exits
	// non-zero because the planner refuses that combination and skips the
	// service. services.yaml can still hold it, which is what this guard covers.
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.AuthService = "docs"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "WITHOUT auth") {
		t.Errorf("the auth service itself must be exempt, got:\n%s", out)
	}
}

// public: true with no label is the §12 gotcha made visible, and the advisory
// must carry the exact label to paste.
func TestDoctorPublic_DeclaredPublicButNotServed(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none") // keep the bypass check out of this test
	setPublic(t, dir, "blog", ptr(true))
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "declares public: true but has no public ingress") {
		t.Errorf("expected declared-but-unserved advisory, got:\n%s", out)
	}
	// Snippet names the container from `backend: ghost:2368` and its port.
	if !strings.Contains(out, `"ghost"`) || !strings.Contains(out, "blog.example.com:2368") {
		t.Errorf("advisory should suggest the ghost container and port, got:\n%s", out)
	}
	if code == 0 {
		t.Error("a violated declaration must be a doctor problem")
	}
}

// For a forward-auth service the suggested snippet must route through Caddy —
// otherwise following the advice would create the auth bypass of the check above.
func TestDoctorPublic_SuggestedSnippetIsAuthAware(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", ptr(true))
	writeCompose(t, dir, "appbox", `services:
  ghost:
    labels:
      cloudflare.io/hostname: "blog.example.com"
`)
	out, _ := doctorOut(t, dir)
	if !strings.Contains(out, "cloudflare.io/reverseproxy") {
		t.Errorf("forward-auth snippet must route through Caddy, got:\n%s", out)
	}
	// Direct-with-port would be the bypass — the port form must NOT appear.
	if strings.Contains(out, "docs.example.com:8000") {
		t.Errorf("forward-auth snippet must not point direct at the container port:\n%s", out)
	}
}

// public: false with a label present is the security-relevant direction:
// exposed against an explicit declaration.
func TestDoctorPublic_DeclaredInternalButExposed(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none") // isolate from the bypass check
	setPublic(t, dir, "docs", ptr(false))
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "declares public: false but IS exposed") {
		t.Errorf("expected exposed-against-declaration advisory, got:\n%s", out)
	}
	if code == 0 {
		t.Error("exposure against declaration must be a doctor problem")
	}
}

// Undeclared services are silent in BOTH directions — this is what keeps the
// checks opt-in for an existing repo.
func TestDoctorPublic_UndeclaredIsSilent(t *testing.T) {
	dir := doctorSetup(t)
	// Label the NON-auth service: labelling the forward-auth one would (rightly)
	// trip the bypass check and muddy what this test is asserting.
	writeCompose(t, dir, "appbox", `services:
  ghost:
    labels:
      cloudflare.io/hostname: "blog.example.com"
`)
	out, code := doctorOut(t, dir)
	if strings.Contains(out, "declares public") {
		t.Errorf("undeclared services must produce no declaration advisory, got:\n%s", out)
	}
	if code != 0 {
		t.Errorf("undeclared services must not make doctor fail, exit %d:\n%s", code, out)
	}
}

// A publicly-served hostname in a managed domain with no service entry has no
// internal horizon. It is informational only — it must NOT fail doctor.
func TestDoctorPublic_OrphanIngress(t *testing.T) {
	dir := doctorSetup(t)
	writeCompose(t, dir, "appbox", `services:
  anisette:
    labels:
      cloudflare.io/hostname: "anisette.example.com:8080"
`)
	out, code := doctorOut(t, dir)
	// Singular subject takes a singular verb ("1 hostname has", not "have").
	if !strings.Contains(out, "1 public hostname on appbox has no internal horizon") {
		t.Errorf("expected the orphan-ingress advisory, got:\n%s", out)
	}
	// The suggested command should be complete: name, fqdn, host, backend:port.
	if !strings.Contains(out, "hemma add service anisette --fqdn anisette.example.com --host appbox --backend anisette:8080") {
		t.Errorf("orphan advisory should carry a ready add command, got:\n%s", out)
	}
	if code != 0 {
		t.Errorf("orphan ingress is informational and must not fail doctor, exit %d", code)
	}
}

// Hostnames outside the managed domains are none of hemma's business — a
// homelab compose file legitimately serves other zones.
func TestDoctorPublic_OrphanIgnoresUnmanagedDomains(t *testing.T) {
	dir := doctorSetup(t)
	writeCompose(t, dir, "appbox", `services:
  other:
    labels:
      cloudflare.io/hostname: "thing.elsewhere.net"
`)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "internal horizon") {
		t.Errorf("unmanaged domain must be ignored, got:\n%s", out)
	}
}

// A missing compose file yields no advisories at all: absence of evidence is
// not evidence of misconfiguration.
func TestDoctorPublic_UnreadableComposeIsSilent(t *testing.T) {
	dir := doctorSetup(t)
	setPublic(t, dir, "docs", ptr(true))
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "declares public") || strings.Contains(out, "internal horizon") {
		t.Errorf("no compose file must yield no public-horizon advisories, got:\n%s", out)
	}
}

// An unparseable compose file is equally silent — a YAML syntax error must not
// be reported as "your service is not exposed".
func TestDoctorPublic_UnparseableComposeIsSilent(t *testing.T) {
	dir := doctorSetup(t)
	setPublic(t, dir, "docs", ptr(true))
	writeCompose(t, dir, "appbox", "services: [broken: yaml\n")
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "declares public") {
		t.Errorf("unparseable compose must be silent, got:\n%s", out)
	}
}

// public_label: none switches every public-horizon check off, labels or not.
func TestDoctorPublic_DisabledByConfig(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", ptr(false))
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.PublicLabel = config.PublicLabelDisabled
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := doctorOut(t, dir)
	for _, s := range []string{"WITHOUT auth", "declares public", "internal horizon"} {
		if strings.Contains(out, s) {
			t.Errorf("public_label: none must silence %q, got:\n%s", s, out)
		}
	}
}

// public_proxy_label: none disables ONLY the auth-bypass check; the
// declared-vs-observed check keeps working.
func TestDoctorPublic_ProxyLabelDisabledKeepsOtherChecks(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", ptr(false))
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.PublicProxyLabel = config.PublicLabelDisabled
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "WITHOUT auth") {
		t.Errorf("proxy label disabled must silence the bypass check, got:\n%s", out)
	}
	if !strings.Contains(out, "declares public: false but IS exposed") {
		t.Errorf("the declaration check must still run, got:\n%s", out)
	}
}

// A disabled service generates no Caddy block, so its label state says nothing
// about hemma's config — it must be skipped entirely.
func TestDoctorPublic_SkipsDisabledServices(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", ptr(false))
	if code := Run([]string{"-C", dir, "disable", "service", "docs"}); code != 0 {
		t.Fatalf("disable exit %d", code)
	}
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "WITHOUT auth") || strings.Contains(out, "declares public") {
		t.Errorf("disabled service must be skipped, got:\n%s", out)
	}
}

// The doctor checks must never write the compose file they read.
func TestDoctorPublic_NeverWritesCompose(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "blog", ptr(true))
	body := `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
  orphan:
    labels:
      cloudflare.io/hostname: "extra.example.com"
`
	writeCompose(t, dir, "appbox", body)
	path := filepath.Join(dir, "appbox", composeFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		Run([]string{"-C", dir, "doctor"})
		Run([]string{"-C", dir, "doctor", "--fix"})
	})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("doctor rewrote the compose file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("compose content changed:\n%s", got)
	}
}

// containerAndPort splits a backend into the container to label and its port.
func TestContainerAndPort(t *testing.T) {
	for in, want := range map[string][2]string{
		"paperless:8000":            {"paperless", "8000"},
		"host.docker.internal:8123": {"host.docker.internal", "8123"},
		"bare":                      {"bare", ""},
	} {
		c, p := containerAndPort(in)
		if c != want[0] || p != want[1] {
			t.Errorf("containerAndPort(%q) = (%q, %q), want (%q, %q)", in, c, p, want[0], want[1])
		}
	}
}

// suggestName derives a service name from a hostname's first label.
func TestSuggestName(t *testing.T) {
	if got := suggestName("status.guldmund.dk"); got != "status" {
		t.Errorf("suggestName = %q, want status", got)
	}
}

// Two orphans on one host produce ONE advisory listing both, with plural
// agreement — N separate advisories would bury the rest of doctor's output.
func TestDoctorPublic_OrphansGroupedPerHostWithPluralAgreement(t *testing.T) {
	dir := doctorSetup(t)
	writeCompose(t, dir, "appbox", `services:
  one:
    labels:
      cloudflare.io/hostname: "one.example.com:81"
  two:
    labels:
      cloudflare.io/hostname: "two.example.com:82"
`)
	out, _ := doctorOut(t, dir)
	if !strings.Contains(out, "2 public hostnames on appbox have no internal horizon") {
		t.Errorf("expected one grouped plural advisory, got:\n%s", out)
	}
	// Count HEADLINES, not the phrase — the body repeats "no internal horizon".
	if n := strings.Count(out, "public hostnames on appbox"); n != 1 {
		t.Errorf("expected exactly 1 grouped advisory, got %d:\n%s", n, out)
	}
	for _, want := range []string{"one.example.com", "two.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("advisory should list %s, got:\n%s", want, out)
		}
	}
}
