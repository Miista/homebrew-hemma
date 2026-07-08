package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if code := Run([]string{"-C", dir, "set", "auth-snippet", snippet}); code != 0 {
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

// The AUTH column shows ✓ for an auth service and - for a non-auth one, and the
// disable hint appears because at least one service uses auth.
func TestList_AuthColumn(t *testing.T) {
	dir := listSetup(t, "snip.caddy")
	out := captureStdout(t, func() { Run([]string{"-C", dir, "list", "--all"}) })

	// Header carries the AUTH column.
	if !strings.Contains(out, "AUTH") {
		t.Errorf("expected AUTH header column, got:\n%s", out)
	}
	// Find the two service rows and assert their trailing auth glyph.
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
	if !strings.HasSuffix(strings.TrimRight(docsLine, " "), "✓") {
		t.Errorf("auth service row should end in ✓, got %q", docsLine)
	}
	if !strings.HasSuffix(strings.TrimRight(blogLine, " "), "-") {
		t.Errorf("non-auth service row should end in -, got %q", blogLine)
	}
	if !strings.Contains(out, "imports the auth snippet") {
		t.Errorf("expected the disable hint when a service uses auth, got:\n%s", out)
	}
}

// The completion command prints a script for bash/zsh and rejects others.
func TestRun_Completion(t *testing.T) {
	bash := captureStdout(t, func() {
		if code := Run([]string{"completion", "bash"}); code != 0 {
			t.Fatalf("completion bash exit %d", code)
		}
	})
	if !strings.Contains(bash, "complete -F _splitdns splitdns") {
		t.Errorf("bash script missing complete registration:\n%s", bash)
	}
	zsh := captureStdout(t, func() {
		if code := Run([]string{"completion", "zsh"}); code != 0 {
			t.Fatalf("completion zsh exit %d", code)
		}
	})
	if !strings.Contains(zsh, "#compdef splitdns") {
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
