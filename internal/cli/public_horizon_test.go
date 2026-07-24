package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// writeCompose drops a docker-compose.yml into a host's repo directory.
func writeCompose(t *testing.T, dir, host, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, host, composeFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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

// The PUBLIC column marks a labelled FQDN public and an unlabelled one local.
// Only the label VALUE matters, not which container carries it: here the label
// sits on a caddy container, not on the service's own.
func TestList_PublicColumn(t *testing.T) {
	dir := listSetup(t, "")
	writeCompose(t, dir, "appbox", `services:
  caddy:
    image: caddy
    labels:
      cloudflare.io/hostname: "docs.example.com"
  ghost:
    image: ghost
`)
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })

	if !strings.Contains(out, "PUBLIC") {
		t.Fatalf("expected PUBLIC header, got:\n%s", out)
	}
	docs := serviceLine(out, "docs.example.com")
	blog := serviceLine(out, "blog.example.com")
	if got := strings.Fields(docs); len(got) == 0 || got[len(got)-1] != publicYes {
		t.Errorf("labelled service should be %s, got %q", publicYes, docs)
	}
	if got := strings.Fields(blog); len(got) == 0 || got[len(got)-1] != publicNo {
		t.Errorf("unlabelled service should be %s, got %q", publicNo, blog)
	}
}

// The list form of compose labels (- key=value) is accepted, and a backend-port
// suffix on the label value is stripped before matching.
func TestList_PublicListFormAndPortSuffix(t *testing.T) {
	dir := listSetup(t, "")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    image: paperless
    labels:
      - cloudflare.io/hostname=docs.example.com:8000
`)
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if got := strings.Fields(serviceLine(out, "docs.example.com")); len(got) == 0 || got[len(got)-1] != publicYes {
		t.Errorf("list-form label with port suffix should be %s, got:\n%s", publicYes, out)
	}
}

// A configured public_label overrides the cloudflared default, so a different
// tunnel tool's convention works without code changes.
func TestList_PublicCustomLabel(t *testing.T) {
	dir := listSetup(t, "")
	writeCompose(t, dir, "appbox", `services:
  paperless:
    labels:
      my.tunnel/host: "docs.example.com"
`)
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.PublicLabel = "my.tunnel/host"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if got := strings.Fields(serviceLine(out, "docs.example.com")); len(got) == 0 || got[len(got)-1] != publicYes {
		t.Errorf("custom public_label should mark the service %s, got:\n%s", publicYes, out)
	}
	// The default key must no longer be consulted.
	if strings.Contains(out, config.DefaultPublicLabel) {
		t.Errorf("footnote should name the configured label, not the default:\n%s", out)
	}
}

// public_label: none switches the column off entirely, even with compose
// labels present.
func TestList_PublicDisabled(t *testing.T) {
	dir := listSetup(t, "")
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
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if strings.Contains(out, "PUBLIC") {
		t.Errorf("public_label: none must drop the column, got:\n%s", out)
	}
}

// With no compose file anywhere, the column is dropped rather than rendering a
// useless column of "?" — a repo whose hosts are not compose-managed.
func TestList_PublicHiddenWithoutCompose(t *testing.T) {
	dir := listSetup(t, "")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if strings.Contains(out, "PUBLIC") {
		t.Errorf("no compose file must drop the column, got:\n%s", out)
	}
}

// One readable compose file is enough to show the column; services on a host
// whose compose is missing then read as unknown rather than silently local.
func TestList_PublicUnknownPerHost(t *testing.T) {
	dir := listSetup(t, "")
	if code := Run([]string{"-C", dir, "add", "service", "dns",
		"--fqdn", "dns.example.com", "--host", "resolver", "--backend", "pihole:80"}); code != 0 {
		t.Fatalf("add resolver service exit %d", code)
	}
	writeCompose(t, dir, "appbox", `services:
  caddy:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`)
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })
	if got := strings.Fields(serviceLine(out, "dns.example.com")); len(got) == 0 || got[len(got)-1] != publicUnknown {
		t.Errorf("service on compose-less host should be %s, got:\n%s", publicUnknown, out)
	}
	if !strings.Contains(out, "could not be read") {
		t.Errorf("expected the unknown-exposure footnote, got:\n%s", out)
	}
}

// An unparseable compose file is unknown, not "not public" — a YAML error must
// never be read as a fact about exposure.
func TestLabelledIngress_UnparseableIsUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, composeFile)
	if err := os.WriteFile(path, []byte("services: [this is: not valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := labelledIngress(path, config.DefaultPublicLabel, config.DefaultPublicProxyLabel); got != nil {
		t.Errorf("unparseable compose should be nil (unknown), got %v", got)
	}
	if got := labelledIngress(filepath.Join(dir, "absent.yml"), config.DefaultPublicLabel, config.DefaultPublicProxyLabel); got != nil {
		t.Errorf("missing compose should be nil (unknown), got %v", got)
	}
}

// labelledIngress captures the container, the port suffix, and whether a proxy
// label routes the hostname through a reverse proxy — the three facts the doctor
// checks are built on. container_name overrides the compose service key.
func TestLabelledIngress_CapturesContainerPortAndProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, composeFile)
	if err := os.WriteFile(path, []byte(`services:
  gatus:
    labels:
      cloudflare.io/hostname: "status.example.com"
      cloudflare.io/reverseproxy: "https://caddy:443"
  ha:
    container_name: homeassistant
    labels:
      cloudflare.io/hostname: "ha.example.com:8123"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := labelledIngress(path, config.DefaultPublicLabel, config.DefaultPublicProxyLabel)
	if in := got["status.example.com"]; in.Container != "gatus" || !in.Proxied || in.Port != "" {
		t.Errorf("gatus ingress wrong: %+v", in)
	}
	if in := got["ha.example.com"]; in.Container != "homeassistant" || in.Proxied || in.Port != "8123" {
		t.Errorf("ha ingress wrong (container_name should win): %+v", in)
	}
	// With the proxy label disabled, nothing is Proxied — which switches the
	// auth-bypass check off rather than making it fire on everything.
	off := labelledIngress(path, config.DefaultPublicLabel, "")
	if off["status.example.com"].Proxied {
		t.Error("empty proxyLabel must leave Proxied false")
	}
}

// Hostname matching is case-insensitive on both sides.
func TestHostnameFromLabel(t *testing.T) {
	for in, want := range map[string]string{
		"App.Example.COM":       "app.example.com",
		" app.example.com:8123": "app.example.com",
		"app.example.com":       "app.example.com",
		"":                      "",
	} {
		if got := hostnameFromLabel(in); got != want {
			t.Errorf("hostnameFromLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// The PUBLIC read must never modify the compose file it inspects. hemma does
// not own docker-compose.yml (design §12) and the "never write compose" line is
// absolute — this pins it, since public_horizon.go is one of only two places in the
// codebase that constructs a compose path.
func TestList_NeverWritesComposeFile(t *testing.T) {
	dir := listSetup(t, "")
	body := `services:
  caddy:
    labels:
      cloudflare.io/hostname: "docs.example.com"
`
	writeCompose(t, dir, "appbox", body)
	path := filepath.Join(dir, "appbox", composeFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Exercise every command that could plausibly touch a host directory.
	captureStdout(t, func() {
		Run([]string{"-C", dir, "list", "--all"})
		Run([]string{"-C", dir, "update", "service", "docs", "--backend", "paperless:8001"})
		Run([]string{"-C", dir, "doctor", "--fix"})
	})

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("compose file must still exist: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("compose file was rewritten (mtime %v -> %v)", before.ModTime(), after.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("compose content changed:\n--- want ---\n%s\n--- got ---\n%s", body, got)
	}
}

// An empty or whitespace-only label value declares no hostname and must be
// skipped rather than entering the map under the empty key — which would make
// every service with an empty FQDN look publicly served.
func TestLabelledIngress_SkipsEmptyLabelValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, composeFile)
	if err := os.WriteFile(path, []byte(`services:
  a:
    labels:
      cloudflare.io/hostname: ""
  b:
    labels:
      cloudflare.io/hostname: "   "
  c:
    labels:
      cloudflare.io/hostname: "real.example.com"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := labelledIngress(path, config.DefaultPublicLabel, config.DefaultPublicProxyLabel)
	if len(got) != 1 {
		t.Errorf("empty label values must be skipped, got %d entries: %v", len(got), got)
	}
	if _, bad := got[""]; bad {
		t.Error("the empty hostname must never be a key")
	}
}
