package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"hemma/internal/cli/kernel"
	"hemma/internal/config"
)

// The recovery path. When services.yaml cannot be loaded, none of the typed commands can run (they all start
// by loading it), yet the person must still be able to fix it — so the file is offered as text. Reading it is
// plain; WRITING it goes through kernel.ReplaceConfig like every other change (the UI never writes
// services.yaml itself), which refuses text that does not parse, so the UI cannot make things worse.

// handleRawGet returns the file's text and, if it does not load, why.
func (s *server) handleRawGet(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.repo, "services.yaml")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	out := map[string]any{"text": string(b), "error": ""}
	if _, perr := config.Parse(b, path); perr != nil {
		out["error"] = perr.Error()
	}
	writeJSON(w, 200, out)
}

// handleRawSave replaces the file with the posted text: {"text": "..."}.
func (s *server) handleRawSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "invalid request body"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := workContext(r, false, 2*time.Minute) // a save is not abandoned half-way when the browser goes
	defer cancel()
	before := s.configStamp()
	code, out := s.inKernel(ctx, "replace config", func(env *kernel.Env) int {
		return kernel.ReplaceConfig(env, s.repo, filepath.Join(s.repo, "services.yaml"), in.Text)
	})
	changed := s.configStamp() != before
	log.Printf("replace services.yaml -> exit=%d changed=%v", code, changed)
	// A refusal (text that does not parse) is the person's to fix: 400, like any rejected input.
	status := 200
	if code == 2 && !changed {
		status = 400
	}
	writeJSON(w, status, map[string]any{"ok": code == 0, "output": out, "changed": changed})
}
