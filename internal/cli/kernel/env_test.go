package kernel

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A command run with a custom Env prints into that Env's writers and nowhere
// else — this is what lets a non-CLI front end (the web UI) call the kernel
// in-process and capture the result.
func TestEnv_CapturesOutputAndTouchesNoProcessStdio(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)

	var out, errw bytes.Buffer
	env := &Env{Out: &out, Err: &errw, NoTerminal: true}
	var code int
	var leakedStdout string
	leakedStderr := captureStderr(t, func() {
		leakedStdout = captureStdout(t, func() {
			code, _ = Route(env, dir, filepath.Join(dir, configName), "list", []string{"--all"})
		})
	})
	if code != 0 {
		t.Fatalf("list exit %d, stderr %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "== Hosts (2) ==") {
		t.Errorf("Env.Out missing the listing:\n%s", out.String())
	}
	if leakedStdout != "" || leakedStderr != "" {
		t.Errorf("command wrote to the process's stdio: stdout %q stderr %q", leakedStdout, leakedStderr)
	}

	// Errors go to Env.Err, not Env.Out.
	out.Reset()
	errw.Reset()
	code, _ = Route(env, dir, filepath.Join(dir, configName), "add", []string{"service", "x"})
	if code == 0 || !strings.Contains(errw.String(), "Error:") || out.Len() != 0 {
		t.Errorf("error should land on Env.Err only: code %d out %q err %q", code, out.String(), errw.String())
	}
}

// NoTerminal makes the interactive editors refuse even if the process happens
// to have a terminal on stdin.
func TestEnv_NoTerminalRefusesInteractiveEditors(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "resolver", "appbox")
	seed(t, dir)
	var out, errw bytes.Buffer
	env := &Env{Out: &out, Err: &errw, NoTerminal: true}
	for _, args := range [][]string{{"service", "x"}, {"host", "resolver"}} {
		out.Reset()
		errw.Reset()
		if code, _ := Route(env, dir, filepath.Join(dir, configName), "update", args); code == 0 {
			t.Errorf("update %v: want refusal without flags, got exit 0", args)
		}
	}
	if code, _ := Route(env, dir, filepath.Join(dir, configName), "create", []string{"user", "bob"}); code == 0 {
		t.Error("create user: want refusal when NoTerminal")
	}
}

// Cancelling Env.Ctx kills the external command a kernel helper started, so a
// hung docker/ssh cannot hold a front end's lock forever.
func TestEnv_ContextKillsExternalCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out, errw bytes.Buffer
	env := &Env{Out: &out, Err: &errw, Ctx: ctx}
	start := time.Now()
	ok := runQuiet(env, "sleep", "30")
	if ok {
		t.Error("a killed command must report failure")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("command was not cancelled: took %s", d)
	}
}

// Separate Envs run concurrently without their output interleaving or racing
// (run under -race).
func TestEnv_ConcurrentRunsKeepOutputSeparate(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		dir := t.TempDir()
		mkdirs(t, dir, "resolver", "appbox")
		seed(t, dir)
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			var out, errw bytes.Buffer
			env := &Env{Out: &out, Err: &errw, NoTerminal: true}
			name := "svc" + string(rune('a'+i))
			Route(env, dir, filepath.Join(dir, configName), "add",
				[]string{"service", name, "--fqdn", name + ".example.com", "--host", "appbox", "--backend", name + ":80"})
			out.Reset()
			Route(env, dir, filepath.Join(dir, configName), "list", []string{"--all"})
			results[i] = out.String()
		}(i, dir)
	}
	wg.Wait()
	for i, got := range results {
		own := "svc" + string(rune('a'+i))
		if !strings.Contains(got, own) {
			t.Errorf("run %d: output missing its own service %q:\n%s", i, own, got)
		}
		for j := 0; j < n; j++ {
			if j != i && strings.Contains(got, "svc"+string(rune('a'+j))+".example.com") {
				t.Errorf("run %d: output contains run %d's service", i, j)
			}
		}
	}
}
