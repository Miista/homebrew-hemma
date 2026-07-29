package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/config"
)

// captureStderr runs f with os.Stderr redirected to a pipe and returns what
// was written.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	w.Close()
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

func TestRunQuietSuccessIsSilent(t *testing.T) {
	out := captureStderr(t, func() {
		if !runQuiet("sh", "-c", "echo noisy stdout; echo noisy stderr >&2; exit 0") {
			t.Error("expected success")
		}
	})
	if out != "" {
		t.Errorf("success should print nothing, got %q", out)
	}
}

func TestRunQuietFailurePrintsCapturedOutput(t *testing.T) {
	out := captureStderr(t, func() {
		if runQuiet("sh", "-c", "echo diag line 1; echo diag line 2 >&2; exit 1") {
			t.Error("expected failure")
		}
	})
	for _, want := range []string{"    diag line 1", "    diag line 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure output missing %q, got %q", want, out)
		}
	}
}

// runQuietIn must run the command IN the given directory — the whole reason
// it exists over plain runQuiet is that `docker compose` resolves its
// project (and so which docker-compose.override.yml gets merged) from cwd.
func TestRunQuietIn_UsesGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !runQuietIn(dir, "sh", "-c", "test -f marker.txt") {
		t.Error("expected the command to find marker.txt in dir")
	}
	if runQuietIn(t.TempDir(), "sh", "-c", "test -f marker.txt") {
		t.Error("a different (empty) directory should not find marker.txt")
	}
}

// An empty dir must behave exactly like runQuiet (inherit the caller's cwd) —
// runQuiet is defined purely as runQuietIn("", ...), so this pins that they
// stay equivalent rather than silently diverging on a future edit.
func TestRunQuietIn_EmptyDirMatchesRunQuiet(t *testing.T) {
	if runQuiet("sh", "-c", "exit 1") != runQuietIn("", "sh", "-c", "exit 1") {
		t.Error("runQuietIn(\"\", ...) should behave identically to runQuiet")
	}
	if runQuiet("sh", "-c", "exit 0") != runQuietIn("", "sh", "-c", "exit 0") {
		t.Error("runQuietIn(\"\", ...) should behave identically to runQuiet")
	}
}

// hostHasPublicService mirrors planComposeOverrides' own condition: true iff
// that host's docker-compose.override.yml exists on disk (the file is omitted
// entirely for a host with no public: true service, never written empty).
func TestHostHasPublicService(t *testing.T) {
	dir := listSetup(t, "")
	if code := Run([]string{"-C", dir, "set", "tunnel-dir", "cloudflared"}); code != 0 {
		t.Fatalf("set tunnel-dir exit %d", code)
	}
	// listSetup's "docs" is not public; "blog" is not public either — neither
	// host should have an override file yet.
	if hostHasPublicService(dir, mustLoad(t, dir), "appbox") {
		t.Error("no public: true service on appbox yet — should be false")
	}

	if code := Run([]string{"-C", dir, "update", "service", "docs", "--public"}); code != 0 {
		t.Fatalf("--public exit %d", code)
	}
	cfg := mustLoad(t, dir)
	if !hostHasPublicService(dir, cfg, "appbox") {
		t.Error("docs is public and synced — appbox should now have an override file")
	}
	if hostHasPublicService(dir, cfg, "resolver") {
		t.Error("resolver has no public service — should stay false")
	}

	// Opting back out: `update service --public=false` runs Incremental sync
	// (it "can't orphan", per runSync's mode rule), so the now-stale override
	// file is left on disk as a reported orphan rather than deleted — only a
	// Complete-mode operation (doctor --fix, here) actually GCs it. Until
	// that runs, hostHasPublicService correctly still reports true: the file
	// really is still there.
	if code := Run([]string{"-C", dir, "update", "service", "docs", "--public=false"}); code != 0 {
		t.Fatalf("--public=false exit %d", code)
	}
	if !hostHasPublicService(dir, mustLoad(t, dir), "appbox") {
		t.Error("Incremental sync must not have deleted the now-orphaned override file yet")
	}
	captureStdout(t, func() { Run([]string{"-C", dir, "doctor", "--fix"}) })
	if hostHasPublicService(dir, mustLoad(t, dir), "appbox") {
		t.Error("doctor --fix (Complete-mode reconcile) should have GC'd the orphaned override file")
	}
}

// mustLoad loads services.yaml from dir or fails the test.
func mustLoad(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "services.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
