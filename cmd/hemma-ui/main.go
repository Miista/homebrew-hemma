// hemma-ui is a web UI for hemma: view, edit, diagnose and deploy. It runs on the deploy host,
// where Deploy fans out over ssh exactly as `hemma deploy` does.
// It never writes generated files or services.yaml itself — every command is
// run from an allowlist (commands.go) through the same kernel the CLI uses
// (internal/cli/kernel, called in-process), so validation and the
// single-writer invariant (design §6) stay exactly as on the CLI.
// Credential minting (create app/user) is deliberately not exposed: its output
// is secret material to paste elsewhere, which a browser can't act on.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miista/authy"

	"hemma/internal/cli"
	"hemma/internal/cli/kernel"
	"hemma/internal/config"
)

// The page, its stylesheet and the Instrument Sans / JetBrains Mono font files (SIL OFL-1.1, licenses alongside) are compiled into the binary.
//
//go:embed static
var static embed.FS

type server struct {
	repo    string
	data    string     // HEMMA_UI_DATA: authy's files and the last deploy's record (lastdeploy.go); "" keeps no record
	version string     // "hemma <version>", shown in the page footer
	mu      sync.Mutex // one mutating hemma invocation at a time
	cc      changesCache
}

// newGate builds the authy gate from HEMMA_UI_* (OIDC when HEMMA_UI_OIDC_* is
// set, otherwise a local account the first visitor creates). This process can
// drive the Docker socket, so unlike a normal authy app it must never run
// ungated: FromEnv returns a nil gate only when HEMMA_UI_AUTH_DISABLED is set,
// and that is refused.
func newGate(ctx context.Context, opts ...authy.Option) (*authy.Gate, error) {
	opts = append([]authy.Option{authy.WithDataDir(env("HEMMA_UI_DATA", "/data/auth"))}, opts...)
	gate, err := authy.FromEnv(ctx, "hemma-ui", opts...)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, errors.New("HEMMA_UI_AUTH_DISABLED is set — refusing to start: hemma-ui controls the Docker socket and must always sit behind a login")
	}
	return gate, nil
}

func main() {
	gate, err := newGate(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	kernel.DisableColor()               // output is captured for a browser, never a terminal
	kernel.SetLocalHost(deployHostSelf) // this page runs on the deploy host (host.go)
	s := &server{repo: env("HEMMA_REPO", "/repo"), data: env("HEMMA_UI_DATA", "/data/auth"), version: "hemma " + cli.Version}
	s.markInterruptedDeploy()
	handler, err := s.routes(gate)
	if err != nil {
		log.Fatal(err)
	}
	addr := env("HEMMA_UI_ADDR", ":8080")
	log.Printf("hemma-ui listening on %s (repo %s)", addr, s.repo)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// routes is the whole HTTP surface. The authy gate is the outermost layer:
// nothing below it (not even the page or the static files) is reachable
// without a session.
func (s *server) routes(gate *authy.Gate) (http.Handler, error) {
	mux := http.NewServeMux()
	page, err := static.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	page = versionIcons(page)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	assets, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/signin", s.signin)
	mux.HandleFunc("GET /api/commands", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, commands) })
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("GET /api/changes", s.handleChanges)
	mux.HandleFunc("GET /api/diff", s.handleDiff)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/discard", s.handleDiscard)
	mux.HandleFunc("POST /api/commit", s.handleCommit)
	mux.HandleFunc("POST /api/run", s.runCommand)
	mux.HandleFunc("POST /api/deploy", s.handleDeploy)
	mux.HandleFunc("GET /api/issues", s.handleIssues)
	mux.HandleFunc("GET /api/config/raw", s.handleRawGet)
	mux.HandleFunc("POST /api/config/raw", s.handleRawSave)
	return gate.Wrap(guard(mux)), nil
}

// versionIcons gives the page's icon links a hash of each file (?v=…). Browsers keep favicons in a store of their own that
// ignores Cache-Control, so a changed icon only shows up under a new URL. The file server ignores the query.
func versionIcons(page []byte) []byte {
	for _, name := range []string{"icon.svg", "favicon.ico"} {
		b, err := static.ReadFile("static/icons/" + name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		href := `href="/static/icons/` + name
		page = bytes.ReplaceAll(page, []byte(href+`"`), []byte(href+"?v="+hex.EncodeToString(sum[:6])+`"`))
	}
	return page
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// guard sits behind the authy gate. For mutating requests it adds a same-origin
// check (defence in depth next to the SameSite session cookie) and a body size
// cap. Responses are never cached: command output can include config and file
// paths.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			limit := int64(1 << 16)
			if r.URL.Path == "/api/config/raw" {
				limit = 1 << 20 // a whole services.yaml
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		if strings.HasPrefix(r.URL.Path, "/static/fonts/") {
			w.Header().Set("Cache-Control", "private, max-age=86400") // fixed font files: no need to refetch on every load
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// authBypass is where the effective policy skips sign-in (the sign-in icon): "local", or "everywhere" — no sign-in at all, like
// mode none. Resolved by config's own rule (an unset public follows local without the tunnel), so the page never re-derives it.
func authBypass(a config.Auth, tunnel bool) string {
	eff := a.Policy.ForService(tunnel)
	switch {
	case !a.Mode.UsesProvider() || eff.Local != config.PolicyBypass:
		return ""
	case eff.Public == config.PolicyBypass:
		return "everywhere"
	}
	return "local"
}

// signin answers authBypass for the values a service form holds right now, so its sign-in icon follows the form before a save.
// Read-only: nothing is loaded or written. A value hemma would refuse simply is not bypass; the save itself validates.
func (s *server) signin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a := config.Auth{Mode: config.AuthMode(q.Get("mode")),
		Policy: config.AuthPolicy{Local: config.PolicyLevel(q.Get("local")), Public: config.PolicyLevel(q.Get("public"))}}
	writeJSON(w, 200, map[string]string{"auth-bypass": authBypass(a, q.Get("tunnel") == "true")})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		// The version rides along so the recovery screen can still show it in the footer.
		writeJSON(w, 500, map[string]any{"error": err.Error(), "version": s.version, "deploy": s.deployState()})
		return
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	svcs := []map[string]any{}
	for _, n := range names {
		v := cfg.Services[n]
		svcs = append(svcs, map[string]any{
			"name": n, "fqdn": v.FQDN, "host": v.Host, "backend": v.Backend, "disabled": v.Disabled,
			"auth-mode": string(v.Auth.Mode), "auth-groups": strings.Join(v.Auth.Groups, ","), "auth-bypass": authBypass(v.Auth, v.Tunnel),
			"bypass-paths":       strings.Join(v.Auth.BypassPaths, ","),
			"auth-policy-local":  string(v.Auth.Policy.Local),
			"auth-policy-public": string(v.Auth.Policy.Public), "tunnel": v.Tunnel,
		})
	}
	hosts := []map[string]any{}
	hn := make([]string, 0, len(cfg.Hosts))
	for n := range cfg.Hosts {
		hn = append(hn, n)
	}
	sort.Strings(hn)
	for _, n := range hn {
		h := cfg.Hosts[n]
		hosts = append(hosts, map[string]any{"name": n, "ip": h.IP, "ssh": h.SSH,
			"trusted-proxies": strings.Join(h.TrustedProxies, ",")})
	}
	d := cfg.Defaults
	values := map[string]string{
		"dns-host": d.DNSHost, "deploy-host": d.DeployHost, "auth-snippet": d.AuthSnippet,
		"auth-service": d.AuthService, "local-networks": strings.Join(d.LocalNetworks, ","),
		"tunnel-dir": d.TunnelDir,
	}
	settings := []map[string]any{}
	for _, ds := range defaultSettings {
		settings = append(settings, map[string]any{"key": ds.Key, "label": ds.Label, "help": ds.Help,
			"kind": ds.Kind, "clearable": ds.Clearable, "value": values[ds.Key]})
	}
	// What a deploy would do on THIS machine, from the same rules `apply` follows (never a guess in the page).
	selfHost, steps, _ := kernel.ApplySteps(s.repo, filepath.Join(s.repo, "services.yaml"))
	if steps == nil {
		steps = []string{}
	}
	writeJSON(w, 200, map[string]any{
		"apply":    map[string]any{"host": selfHost, "steps": steps},
		"services": svcs, "hosts": hosts, "domains": cfg.DomainNames(), "settings": settings, "version": s.version,
		"deploy": s.deployState(), // the last deploy, for a page whose stream was cut (lastdeploy.go)
	})
}

// runCommand runs one allowlisted hemma command: {"command":"add host","values":{...}}.
func (s *server) runCommand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Command string         `json:"command"`
		Values  map[string]any `json:"values"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "invalid request body"})
		return
	}
	c := lookup(in.Command)
	if c == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": "unknown command"})
		return
	}
	argv, err := c.build(in.Values)
	if err == nil {
		err = checkAuthFields(c, in.Values)
	}
	if err == nil {
		err = s.checkState(c, in.Values)
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": err.Error()})
		return
	}
	if makesLive(c.Key) {
		if err := s.needDeployHost(); err != nil {
			writeJSON(w, 409, map[string]any{"ok": false, "noDeployHost": true, "output": err.Error()})
			return
		}
	}
	// doctor only READS unless asked to fix: an audit must not take the write lock (it runs after every change, to keep
	// the Issues badge current, and must never make a quick toggle wait) nor outlive the browser.
	readOnly := c.ReadOnly
	if fix, _ := in.Values["fix"].(bool); c.Key == "doctor" && !fix {
		readOnly = true
	}
	if !readOnly {
		s.mu.Lock()
		defer s.mu.Unlock()
		defer s.invalidateChanges() // the repo may have changed: the next page load must see it
	}
	if gated(c) {
		if ch := s.changes(); len(ch.Files) > 0 {
			writeJSON(w, 409, map[string]any{"ok": false, "dirty": true,
				"output": "Refused: there are uncommitted changes. Commit them with a change message first."})
			return
		}
	}
	ctx, cancel := workContext(r, readOnly, c.Timeout)
	defer cancel()
	before := s.configStamp()
	code, out := s.runKernel(ctx, argv)
	ok := code == 0
	if ctx.Err() != nil {
		ok = false
		out += "\n(timed out)\n"
	}
	// A non-zero exit does not mean "nothing happened": hemma saves a change and THEN reports entries it
	// skipped. The page needs to tell "refused, nothing saved" (stay in the form) from "saved, with
	// problems" (close the form, refresh, warn), so say whether services.yaml moved.
	changed := !readOnly && s.configStamp() != before
	// Log the command key only: arguments and output can carry secrets.
	log.Printf("run %q -> exit=%d changed=%v", c.Key, code, changed)
	writeJSON(w, 200, map[string]any{"ok": ok, "output": out, "changed": changed})
}

// workContext is the context a command runs under. A command that CHANGES something must finish even if the
// browser goes away — a phone locking mid-deploy must not leave the fleet half-converged, or a save half-written —
// so it is cut off from the request and bounded only by its own timeout. A read-only one has nothing to protect and
// stops when the person leaves.
func workContext(r *http.Request, readOnly bool, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := r.Context()
	if !readOnly {
		base = context.WithoutCancel(base)
	}
	return context.WithTimeout(base, timeout)
}

// configStamp fingerprints services.yaml (empty when unreadable) to detect that a command changed it.
func (s *server) configStamp() string {
	b, err := os.ReadFile(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runKernel runs one command (argv as built from the allowlist: command word
// first) in-process through the same kernel the CLI uses, capturing everything
// it prints. stdout and stderr share one buffer so the output reads in order,
// as it did when the hemma binary's combined output was captured.
func (s *server) runKernel(ctx context.Context, argv []string) (code int, output string) {
	return s.inKernel(ctx, argv[0], func(kenv *kernel.Env) int {
		code, known := kernel.Route(kenv, s.repo, filepath.Join(s.repo, "services.yaml"), argv[0], argv[1:])
		if !known {
			fmt.Fprintln(kenv.Err, "unknown command")
			return 2
		}
		return code
	})
}

// inKernel runs fn with a capturing Env (and the request's deadline), recovering from a panic so a bug in a
// command cannot take the whole UI down. what only names the work in the log.
func (s *server) inKernel(ctx context.Context, what string, fn func(*kernel.Env) int) (code int, output string) {
	var buf syncBuf
	code = s.inKernelTo(ctx, what, &buf, fn)
	return code, buf.String()
}

// inKernelTo is inKernel writing to w (stdout and stderr both, so the output reads in order) — for a command whose
// output the page follows as it is produced.
func (s *server) inKernelTo(ctx context.Context, what string, w io.Writer, fn func(*kernel.Env) int) (code int) {
	kenv := &kernel.Env{Out: w, Err: w, Ctx: ctx, NoTerminal: true}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("kernel panic running %q: %v", what, p)
			fmt.Fprint(w, "\ninternal error\n")
			code = 70
		}
	}()
	return fn(kenv)
}

// syncBuf is a mutex-guarded buffer: a command's external processes may write
// to it from more than one goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
