package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// deployRecord is the last deploy this UI ran. The page follows a deploy over a stream, and a stream can be cut while the
// deploy carries on (it runs detached from the request, see workContext): a deploy from outside restarts the very tunnel
// it is streamed through, a phone locks. The page then asks /api/state, which reports this record, to learn how it ended.
// It is kept on the data volume, next to authy's file, so it also survives an update of hemma-ui itself.
type deployRecord struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Running     bool       `json:"running"`
	OK          bool       `json:"ok"`
	Interrupted bool       `json:"interrupted,omitempty"` // hemma-ui stopped while it ran (an update, a restart): the outcome is unknown
	Started     time.Time  `json:"started"`
	Finished    *time.Time `json:"finished,omitempty"`
	Steps       []progItem `json:"steps,omitempty"`
	Output      string     `json:"output,omitempty"`
}

func newDeployRecord(command string) deployRecord {
	b := make([]byte, 8)
	rand.Read(b)
	return deployRecord{ID: hex.EncodeToString(b), Command: command, Running: true, Started: time.Now().UTC()}
}

// deployFile is where the record lives; "" (no data dir, as in tests) keeps nothing.
func (s *server) deployFile() string {
	if s.data == "" {
		return ""
	}
	return filepath.Join(s.data, "last-deploy.json")
}

// saveDeploy writes the record whole (a temp file renamed over the old one), so a reader never sees half of it. Best effort:
// a failed write is logged and the deploy goes on — the record is a convenience for the page, not part of the deploy.
func (s *server) saveDeploy(rec deployRecord) {
	f := s.deployFile()
	if f == "" {
		return
	}
	b, _ := json.Marshal(rec)
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("last deploy: %v", err)
		return
	}
	if err := os.Rename(tmp, f); err != nil {
		log.Printf("last deploy: %v", err)
	}
}

// lastDeploy is the record, or nil when there is none (or it cannot be read).
func (s *server) lastDeploy() *deployRecord {
	f := s.deployFile()
	if f == "" {
		return nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	var rec deployRecord
	if json.Unmarshal(b, &rec) != nil {
		return nil
	}
	return &rec
}

// markInterruptedDeploy runs at startup: a record still "running" belongs to a process that is gone, so nobody will finish
// it. It is marked interrupted, and the page says so instead of waiting for an end that never comes.
func (s *server) markInterruptedDeploy() {
	if rec := s.lastDeploy(); rec != nil && rec.Running {
		rec.Running, rec.Interrupted = false, true
		s.saveDeploy(*rec)
	}
}

// deployState is what /api/state reports: the record without its output when it went well (the output only matters for
// Details after a failure, and the state is fetched after every change).
func (s *server) deployState() *deployRecord {
	rec := s.lastDeploy()
	if rec != nil && rec.OK {
		rec.Output = ""
	}
	return rec
}
