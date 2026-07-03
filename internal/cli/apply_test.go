package cli

import (
	"os"
	"strings"
	"testing"
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
