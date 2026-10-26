package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The last deploy's record is how a page whose stream was cut learns the end: /api/state reports it (without the output
// when it went well), it survives a restart of hemma-ui (it is a file), and a record a stopped process left "running" is
// marked interrupted at startup rather than waited on forever.
func TestLastDeploy_RecordedReportedAndInterrupted(t *testing.T) {
	s := repoWithConfig(t)
	s.data = t.TempDir()
	stateDeploy := func() map[string]any {
		rec := httptest.NewRecorder()
		s.state(rec, httptest.NewRequest("GET", "/api/state", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("state: %v", err)
		}
		d, _ := out["deploy"].(map[string]any)
		return d
	}
	if d := stateDeploy(); d != nil {
		t.Fatalf("no deploy yet: %v", d)
	}

	rec := newDeployRecord("deploy")
	s.saveDeploy(rec)
	if d := stateDeploy(); d["id"] != rec.ID || d["running"] != true {
		t.Errorf("running deploy: %v", d)
	}

	end := time.Now().UTC()
	rec.Running, rec.OK, rec.Finished, rec.Output = false, true, &end, "all fine"
	s.saveDeploy(rec)
	if d := stateDeploy(); d["running"] != false || d["ok"] != true || d["output"] != nil {
		t.Errorf("finished ok (output left out): %v", d)
	}
	rec.OK, rec.Output = false, "✗ optiplex: failed-apply"
	s.saveDeploy(rec)
	if d := stateDeploy(); d["ok"] != false || d["output"] != "✗ optiplex: failed-apply" {
		t.Errorf("failed: the output is reported for Details: %v", d)
	}

	// a process that stopped mid-deploy: the next start marks it interrupted
	running := newDeployRecord("deploy")
	s.saveDeploy(running)
	restarted := &server{repo: s.repo, data: s.data}
	restarted.markInterruptedDeploy()
	if d := stateDeploy(); d["id"] != running.ID || d["running"] != false || d["interrupted"] != true {
		t.Errorf("left running by a stopped process: %v", d)
	}
}

// Without a data dir (tests, a misconfigured container) nothing is written anywhere, and the state just has no deploy.
func TestLastDeploy_NoDataDirKeepsNothing(t *testing.T) {
	s := repoWithConfig(t)
	wd, _ := os.Getwd()
	s.saveDeploy(newDeployRecord("deploy"))
	if _, err := os.Stat(filepath.Join(wd, "last-deploy.json")); err == nil {
		os.Remove(filepath.Join(wd, "last-deploy.json"))
		t.Fatal("wrote a record into the working directory")
	}
	if s.lastDeploy() != nil {
		t.Error("a record without a data dir")
	}
}
