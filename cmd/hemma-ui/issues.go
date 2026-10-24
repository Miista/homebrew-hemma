package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"hemma/internal/cli/kernel"
)

// The Issues page is a checklist of what `hemma doctor` checks. This endpoint runs doctor (read-only: no --fix, no
// lock) and returns its output verbatim, the same read into items (issueitems.go), and whether
// `hemma doctor --fix` could resolve something in it.
//
// "Could --fix resolve something?" is answered from doctor's own wording, not guessed: wherever --fix can help,
// doctor says so in a hint ("Run 'hemma doctor --fix' to …"); the one other line that mentions --fix is the note
// that it does NOT resolve the advisories. TestDoctorFixable_FollowsDoctorsOwnHints pins this to the real output,
// so a change to doctor's wording breaks a test instead of quietly hiding or inventing the Fix button.
func (s *server) handleIssues(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := workContext(r, true, 2*time.Minute)
	defer cancel()
	code, out := s.inKernel(ctx, "doctor", func(env *kernel.Env) int {
		return kernel.Doctor(env, filepath.Join(s.repo, "services.yaml"), nil)
	})
	writeJSON(w, 200, map[string]any{"ok": code == 0, "output": out, "fixable": doctorFixable(out), "items": parseDoctorItems(out)})
}

// doctorFixable reports whether doctor's output offers `hemma doctor --fix` for something.
func doctorFixable(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "doctor --fix") && !strings.Contains(line, "does not resolve") {
			return true
		}
	}
	return false
}
