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

// setPublic sets the `public` opt-in on a service directly through the config
// package (the CLI flag is exercised separately).
func setPublic(t *testing.T, dir, svc string, want bool) {
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
	setPublic(t, dir, "blog", true)
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "is declared public but has no public ingress") {
		t.Errorf("expected declared-but-unserved advisory, got:\n%s", out)
	}
	// Snippet names the container from `backend: ghost:2368` and its port.
	if !strings.Contains(out, `"ghost"`) || !strings.Contains(out, "blog.example.com:2368") {
		t.Errorf("advisory should suggest the ghost container and port, got:\n%s", out)
	}
	// Both resolutions offered: following "add the label" blindly is what puts a
	// service on the internet, so opting back out must be visible too.
	if !strings.Contains(out, "hemma update service blog --public=false") {
		t.Errorf("advisory must also offer opting back out, got:\n%s", out)
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
	setPublic(t, dir, "docs", true)
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

// Exposure with no opt-in is a finding: absent `public` means local-only, so a
// label on such a service made it public without that being written down. This
// is the accidental-exposure check.
func TestDoctorPublic_ExposedWithoutOptIn(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none") // isolate from the bypass check
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "1 service is publicly exposed without `public: true`") {
		t.Errorf("expected the undeclared-exposure advisory, got:\n%s", out)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "docs.example.com") {
		t.Errorf("advisory should name the service and fqdn, got:\n%s", out)
	}
	if code == 0 {
		t.Error("accidental exposure must be a doctor problem")
	}
}

// Opting in resolves it — the same fleet state, now declared, is silent.
func TestDoctorPublic_OptInResolvesUndeclaredExposure(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--public"}); code != 0 {
		t.Fatalf("--public exit %d", code)
	}
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "without `public: true`") {
		t.Errorf("opting in must resolve the finding, got:\n%s", out)
	}
}

// Several undeclared exposures group into ONE advisory with plural agreement —
// this fires in bulk when a repo first adopts the field, and N advisories would
// bury everything else.
func TestDoctorPublic_UndeclaredExposureGrouped(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
  ghost:
    labels:
      cloudflare.io/hostname: "blog.example.com"
`)
	out, _ := doctorOut(t, dir)
	if !strings.Contains(out, "2 services are publicly exposed without `public: true`") {
		t.Errorf("expected one grouped plural advisory, got:\n%s", out)
	}
	if n := strings.Count(out, "publicly exposed without"); n != 1 {
		t.Errorf("expected exactly 1 grouped advisory, got %d:\n%s", n, out)
	}
	// Both resolutions offered, removal first — declaring an accidental label is
	// the wrong repair, so the advisory must not read as "just run this".
	if !strings.Contains(out, "if the exposure is NOT intended, remove the label") {
		t.Errorf("advisory must offer removing the label first, got:\n%s", out)
	}
	for _, want := range []string{"hemma update service docs --public", "hemma update service blog --public"} {
		if !strings.Contains(out, want) {
			t.Errorf("advisory should carry %q, got:\n%s", want, out)
		}
	}
}

// --fix must NOT adopt observed exposure into services.yaml. hemma owns that
// file and could write it, but auto-declaring would silence the alarm in exactly
// the case it exists for.
func TestDoctorPublic_FixDoesNotAdoptExposure(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })
	if declaredOf(t, dir, "docs") {
		t.Error("--fix must not write public: true from an observed label")
	}
	// And the finding survives --fix, so it cannot be silently cleared.
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "without `public: true`") || code == 0 {
		t.Errorf("finding must survive --fix, exit %d:\n%s", code, out)
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
	if !strings.Contains(out, "1 public hostname on appbox has no split-horizon record") {
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
	if strings.Contains(out, "split-horizon record") {
		t.Errorf("unmanaged domain must be ignored, got:\n%s", out)
	}
}

// A missing compose file yields no advisories at all: absence of evidence is
// not evidence of misconfiguration.
func TestDoctorPublic_UnreadableComposeIsSilent(t *testing.T) {
	dir := doctorSetup(t)
	setPublic(t, dir, "docs", true)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "declared public") || strings.Contains(out, "split-horizon record") {
		t.Errorf("no compose file must yield no public-horizon advisories, got:\n%s", out)
	}
}

// An unparseable compose file is equally silent — a YAML syntax error must not
// be reported as "your service is not exposed".
func TestDoctorPublic_UnparseableComposeIsSilent(t *testing.T) {
	dir := doctorSetup(t)
	setPublic(t, dir, "docs", true)
	writeCompose(t, dir, "appbox", "services: [broken: yaml\n")
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "declared public") {
		t.Errorf("unparseable compose must be silent, got:\n%s", out)
	}
}

// public_label: none switches every public-horizon check off, labels or not.
func TestDoctorPublic_DisabledByConfig(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", true)
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
	for _, s := range []string{"WITHOUT auth", "declared public", "split-horizon record"} {
		if strings.Contains(out, s) {
			t.Errorf("public_label: none must silence %q, got:\n%s", s, out)
		}
	}
}

// public_proxy_label: none disables ONLY the auth-bypass check; the
// declared-but-not-served check keeps working.
func TestDoctorPublic_ProxyLabelDisabledKeepsOtherChecks(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "blog", true) // opted in, but no label for blog below
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
	// docs IS served direct and IS forward-auth, but the bypass check is off.
	if strings.Contains(out, "WITHOUT auth") {
		t.Errorf("proxy label disabled must silence the bypass check, got:\n%s", out)
	}
	if !strings.Contains(out, "blog is declared public but has no public ingress") {
		t.Errorf("the opt-in check must still run, got:\n%s", out)
	}
}

// A disabled service generates no Caddy block, so its label state says nothing
// about hemma's config — it must be skipped entirely.
func TestDoctorPublic_SkipsDisabledServices(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "docs", true)
	if code := Run([]string{"-C", dir, "disable", "service", "docs"}); code != 0 {
		t.Fatalf("disable exit %d", code)
	}
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out, _ := doctorOut(t, dir)
	if strings.Contains(out, "WITHOUT auth") || strings.Contains(out, "declared public") {
		t.Errorf("disabled service must be skipped, got:\n%s", out)
	}
}

// The doctor checks must never write the compose file they read.
func TestDoctorPublic_NeverWritesCompose(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "forward")
	setPublic(t, dir, "blog", true)
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
	if !strings.Contains(out, "2 public hostnames on appbox have no split-horizon record") {
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

// declaredOf reads a service's public opt-in straight from the persisted YAML,
// so these tests assert what was actually written.
func declaredOf(t *testing.T, dir, svc string) bool {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.Services[svc]
	if !ok {
		t.Fatalf("service %q missing", svc)
	}
	return s.Public
}

// --public on `add service` opts in at creation time; without it the service is
// local-only and no key is written.
func TestAddService_PublicFlag(t *testing.T) {
	for _, c := range []struct {
		label string
		args  []string
		want  bool
	}{
		{"opted in", []string{"--public"}, true},
		{"explicit true", []string{"--public=true"}, true},
		{"explicit false", []string{"--public=false"}, false},
		{"omitted", nil, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			dir := t.TempDir()
			mkdirs(t, dir, "resolver", "appbox")
			seed(t, dir)
			args := append([]string{"-C", dir, "add", "service", "svc",
				"--fqdn", "svc.example.com", "--host", "appbox", "--backend", "app:1234"}, c.args...)
			if code := Run(args); code != 0 {
				t.Fatalf("add %v exit %d", c.args, code)
			}
			if got := declaredOf(t, dir, "svc"); got != c.want {
				t.Errorf("add %v: Public = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

// --public opts in on update, and --public=false opts back out — omitempty then
// drops the key, so opting out and never having opted in are identical on disk.
func TestUpdateService_PublicFlag(t *testing.T) {
	dir := doctorSetup(t)

	if code := Run([]string{"-C", dir, "update", "service", "blog", "--public"}); code != 0 {
		t.Fatalf("--public exit %d", code)
	}
	if !declaredOf(t, dir, "blog") {
		t.Fatal("--public should opt in")
	}

	if code := Run([]string{"-C", dir, "update", "service", "blog", "--public=false"}); code != 0 {
		t.Fatalf("--public=false exit %d", code)
	}
	if declaredOf(t, dir, "blog") {
		t.Fatal("--public=false should opt back out")
	}
	b, err := os.ReadFile(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "public:") {
		t.Errorf("opting out must remove the key, not write public: false:\n%s", b)
	}
}

// Not passing --public leaves an existing opt-in alone: update only touches the
// fields given on the command line.
func TestUpdateService_PublicUntouchedWhenFlagAbsent(t *testing.T) {
	dir := doctorSetup(t)
	if code := Run([]string{"-C", dir, "update", "service", "blog", "--public"}); code != 0 {
		t.Fatalf("setup --public exit %d", code)
	}
	if code := Run([]string{"-C", dir, "update", "service", "blog", "--backend", "ghost:9999"}); code != 0 {
		t.Fatalf("update backend exit %d", code)
	}
	if !declaredOf(t, dir, "blog") {
		t.Error("an unrelated update must not clear the opt-in")
	}
}

// The flag composes with the check it exists to feed: opt in, then be told the
// public half was never wired.
func TestPublicFlag_FeedsDoctorCheck(t *testing.T) {
	dir := doctorSetup(t)
	setAuthMode(t, dir, "docs", "none")
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--public"}); code != 0 {
		t.Fatalf("--public exit %d", code)
	}
	writeCompose(t, dir, "appbox", `services:
  ghost:
    labels:
      cloudflare.io/hostname: "blog.example.com"
`)
	out, code := doctorOut(t, dir)
	if !strings.Contains(out, "docs is declared public but has no public ingress") {
		t.Errorf("flag-set opt-in should drive the check, got:\n%s", out)
	}
	if code == 0 {
		t.Error("expected non-zero exit")
	}
}
