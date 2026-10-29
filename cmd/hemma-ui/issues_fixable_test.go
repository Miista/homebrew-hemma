package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"hemma/internal/cli/kernel"
)

// A finding is fixable exactly when it carries a fixer, which is what `doctor --fix` runs. That is true by construction, but
// a fixer can still fail to fix what its finding describes. So this builds a repo with a mix of problems, lets --fix loose on
// it, and holds both directions: every finding marked fixable is gone afterwards, and every finding that went away was
// marked fixable (a repair that happens without being announced would hide the Fix button).
func TestIssues_FixableMatchesWhatFixDoes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is needed: doctor asks it which generated files are ignored")
	}
	s := repoWithConfig(t)
	ctx := context.Background()
	must := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = s.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(s.repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("git", "init", "-q")
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if code, out := s.runKernel(ctx, []string{"add", "service", n, "--fqdn", n + ".example.com", "--host", "appbox", "--backend", n + ":80"}); code != 0 {
			t.Fatalf("add %s: %d\n%s", n, code, out)
		}
	}

	// fixable: a generated file gitignored, hemma's own .gitignore block out of date
	// not fixable: blanket data/ rules left outside the block (hemma does not edit what it does not own)
	write(".gitignore", "**/sites/gamma.caddy\n\n# >>> hemma managed >>>\n!**/data/\n!**/data/**/\n# <<< hemma managed <<<\n\n!**/data/\n")
	// fixable: an outdated generated import (appbox), a missing one (resolver)
	// not fixable: the trusted-proxies import missing (resolver) — it belongs in a hand-maintained options block
	write("appbox/caddy/data/Caddyfile", "{\n\tservers {\n\t\timport hemma.trusted_proxies.generated.caddy\n\t}\n}\nimport splitdns.generated.caddy\n")
	write("resolver/caddy/data/Caddyfile", "{\n\tauto_https off\n}\n")
	// fixable: drift — a generated file modified, another one missing
	sites := filepath.Join(s.repo, "appbox", "caddy", "data", "sites")
	f, err := os.OpenFile(filepath.Join(sites, "alpha.caddy"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("# edited by hand\n")
	f.Close()
	if err := os.Remove(filepath.Join(sites, "beta.caddy")); err != nil {
		t.Fatal(err)
	}

	// a title without its counts ("3 generated files …" becomes "N generated files …"), so a finding is the same one after
	digits := regexp.MustCompile(`[0-9]+`)
	findings := func() (fixable, manual map[string]bool) {
		t.Helper()
		fs, err := kernel.Diagnose(kernel.StdEnv(), filepath.Join(s.repo, "services.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		fixable, manual = map[string]bool{}, map[string]bool{}
		for _, f := range fs {
			if f.Status != "warn" && f.Status != "err" {
				continue
			}
			key := digits.ReplaceAllString(f.Title, "N")
			if f.Fixable {
				fixable[key] = true
			} else {
				manual[key] = true
			}
		}
		return
	}

	fixBefore, manualBefore := findings()
	// the test would prove nothing if either side were empty
	if len(fixBefore) == 0 || len(manualBefore) == 0 {
		t.Fatalf("the repo should raise both kinds: fixable %v, manual %v", fixBefore, manualBefore)
	}
	s.runKernel(ctx, []string{"doctor", "--fix"}) // exits non-zero while a finding it cannot repair remains: the exit code is not the point
	fixAfter, manualAfter := findings()

	for k := range fixBefore {
		if fixAfter[k] || manualAfter[k] {
			t.Errorf("marked fixable, but --fix did not fix it: %q", k)
		}
	}
	for k := range manualBefore {
		if !manualAfter[k] && !fixAfter[k] {
			t.Errorf("--fix fixed it, but it was not marked fixable: %q", k)
		}
	}
	for k := range fixAfter {
		if !fixBefore[k] && !manualBefore[k] {
			t.Errorf("--fix introduced a finding: %q", k)
		}
	}
}
