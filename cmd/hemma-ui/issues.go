package main

import (
	"net/http"
	"path/filepath"
	"time"

	"hemma/internal/cli/kernel"
)

// The Issues page is a checklist of what `hemma doctor` checks. This endpoint asks the kernel for doctor's findings
// (kernel.Diagnose: read-only, no --fix, no lock) and returns them as they are — the same findings the CLI prints as text,
// with their own title, summary, steps and files. Nothing is read back out of printed text.
//
// Whether a finding can be fixed automatically is a field of the finding: it is fixable exactly when it carries the fixer that
// `hemma doctor --fix` runs, so the page cannot offer a fix that does not exist or hide one that does
// (TestIssues_FixableMatchesWhatFixDoes holds the two together).
func (s *server) handleIssues(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := workContext(r, true, 2*time.Minute)
	defer cancel()
	var findings []kernel.Finding
	var err error
	s.inKernel(ctx, "doctor", func(env *kernel.Env) int {
		findings, err = kernel.Diagnose(env, filepath.Join(s.repo, "services.yaml"))
		return 0
	})
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "items": []kernel.Finding{}})
		return
	}
	if findings == nil {
		findings = []kernel.Finding{}
	}
	writeJSON(w, 200, map[string]any{"ok": kernel.Problems(findings) == 0, "items": findings})
}
