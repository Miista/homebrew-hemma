package kernel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reported is how many findings say something is wrong, whether or not it makes doctor exit non-zero.
func reported(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if f.Status == "warn" || f.Status == "err" {
			n++
		}
	}
	return n
}

// printed is what the CLI prints for findings, for assertions on wording.
func printed(t *testing.T, fs []Finding) string {
	t.Helper()
	return captureStdout(t, func() { printFindings(stdEnv(), "", fs) })
}

func titles(fs []Finding) string {
	var ts []string
	for _, f := range fs {
		ts = append(ts, f.Title)
	}
	return strings.Join(ts, "\n")
}

// Diagnose is read-only: running it twice on a repo full of problems changes nothing on disk and says the same both times.
func TestDiagnose_ChangesNothing(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "docs:80"})
	os.MkdirAll(filepath.Join(dir, "appbox", "caddy", "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "appbox", "caddy", "data", "Caddyfile"), []byte("import splitdns.generated.caddy\n"), 0o644)
	os.Remove(filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy"))

	snapshot := func() string {
		var sb strings.Builder
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				b, _ := os.ReadFile(p)
				sb.WriteString(p + "\x00" + string(b) + "\x00")
			}
			return nil
		})
		return sb.String()
	}
	before := snapshot()
	first, err := Diagnose(stdEnv(), filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Diagnose(stdEnv(), filepath.Join(dir, configName))
	if snapshot() != before {
		t.Error("Diagnose changed files on disk")
	}
	if titles(first) != titles(second) || reported(first) == 0 {
		t.Errorf("two diagnoses should agree and find problems:\n%s\n--\n%s", titles(first), titles(second))
	}
}

// What a UI draws is the findings themselves: they marshal with the fields the page reads, and fixable is exactly "carries a
// fixer".
func TestFinding_JSONShapeAndFixable(t *testing.T) {
	f := Finding{ID: "x", Status: "warn", Title: "t", Summary: "s", Notes: []string{"n"}, List: []string{"l"},
		Steps: []FindingStep{{Label: "fix", Text: "do it"}}, counts: true, lines: []string{"hidden"}}
	b, _ := json.Marshal(f)
	for _, want := range []string{`"id":"x"`, `"status":"warn"`, `"title":"t"`, `"summary":"s"`, `"notes":["n"]`, `"list":["l"]`, `"steps":[{"label":"fix","text":"do it"}]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), "fixable") || strings.Contains(string(b), "hidden") {
		t.Errorf("an unfixable finding must not say fixable, and CLI text is not for the UI: %s", b)
	}
	g := f.withFix(func(*Env) ([]string, error) { return nil, nil })
	if b, _ := json.Marshal(g); !strings.Contains(string(b), `"fixable":true`) {
		t.Errorf("a finding with a fixer is fixable: %s", b)
	}
}

// Findings come out in a fixed order whatever the map order of the hosts: a CLI that prints hosts in a different order on each
// run is a CLI whose output cannot be compared.
func TestDiagnose_HostsInFixedOrder(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	for _, h := range []string{"resolver", "appbox"} {
		os.MkdirAll(filepath.Join(dir, h, "caddy", "data"), 0o755)
		os.WriteFile(filepath.Join(dir, h, "caddy", "data", "Caddyfile"), []byte("{\n}\n"), 0o644)
	}
	var last string
	for i := 0; i < 20; i++ {
		fs, _ := Diagnose(stdEnv(), filepath.Join(dir, configName))
		var ids []string
		for _, f := range fs {
			ids = append(ids, f.ID)
		}
		got := strings.Join(ids, ",")
		if last != "" && got != last {
			t.Fatalf("order changed between runs:\n%s\n%s", last, got)
		}
		last = got
	}
	if !strings.Contains(last, "caddy.import:appbox,caddy.import:resolver") {
		t.Errorf("hosts should be in name order: %s", last)
	}
}

// --fix repairs what it can and says so first, then reports the repo as it now stands: what is fixed shows as fine, what it
// cannot repair is still there, and the exit code follows what is left.
func TestDoctorFix_ReportsActionsThenTheRepoAsItNowStands(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	Run([]string{"-C", dir, "add", "service", "docs", "--fqdn", "docs.example.com", "--host", "appbox", "--backend", "docs:80"})
	cf := filepath.Join(dir, "appbox", "caddy", "data", "Caddyfile")
	os.MkdirAll(filepath.Dir(cf), 0o755)
	os.WriteFile(cf, []byte("{\n}\nimport splitdns.generated.caddy\n"), 0o644) // outdated import; and no trusted-proxies import
	os.Remove(filepath.Join(dir, "appbox", "caddy", "data", "sites", "docs.caddy"))

	var code int
	out := captureStdout(t, func() { code = Run([]string{"-C", dir, "doctor", "--fix"}) })
	i := strings.Index(out, "rewrote")
	j := strings.Index(out, "Reconciled")
	k := strings.Index(out, "Caddyfile imports hemma.generated.caddy")
	if i < 0 || j < 0 || k < 0 || !(i < k && j < k) {
		t.Errorf("the actions come first, then the report of what is now fine:\n%s", out)
	}
	if !strings.Contains(out, "missing `import hemma.trusted_proxies.generated.caddy`") {
		t.Errorf("what --fix cannot repair is still reported:\n%s", out)
	}
	if code != 0 {
		t.Errorf("only an advisory remains, which does not count: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(cf); !strings.Contains(string(b), "import hemma.generated.caddy") || strings.Contains(string(b), "splitdns") {
		t.Errorf("the Caddyfile was not repaired:\n%s", b)
	}
	// nothing to fix: the same report, and no actions
	out2 := captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })
	if strings.Contains(out2, "rewrote") || strings.Contains(out2, "Reconciled") {
		t.Errorf("a second --fix has nothing to do:\n%s", out2)
	}
}
