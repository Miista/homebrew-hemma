// hemma-ui is a single-host web UI for hemma: view, edit, diagnose and apply.
// It never writes generated files or services.yaml itself — every command is
// run through the hemma binary from an allowlist (commands.go), so validation
// and the single-writer invariant (design §6) stay exactly as on the CLI.
// Credential minting (create app/user) is deliberately not exposed: its output
// is secret material to paste elsewhere, which a browser can't act on.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"hemma/internal/config"
)

// The page, its stylesheet and the Geist font files (SIL OFL-1.1, license alongside) are compiled into the binary.
//
//go:embed static
var static embed.FS

type server struct {
	repo, bin string
	version   string     // `hemma version`, shown in the page footer
	mu        sync.Mutex // one mutating hemma invocation at a time
}

func main() {
	pw := os.Getenv("HEMMA_UI_PASSWORD")
	if pw == "" {
		log.Fatal("HEMMA_UI_PASSWORD must be set — refusing to start without a login")
	}
	s := &server{repo: env("HEMMA_REPO", "/repo"), bin: env("HEMMA_BIN", "hemma")}
	if out, err := exec.Command(s.bin, "version").Output(); err == nil {
		s.version = strings.TrimSpace(string(out))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := static.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	assets, err := fs.Sub(static, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/commands", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, commands) })
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("GET /api/changes", s.handleChanges)
	mux.HandleFunc("GET /api/diff", s.handleDiff)
	mux.HandleFunc("POST /api/discard", s.handleDiscard)
	mux.HandleFunc("POST /api/commit", s.handleCommit)
	mux.HandleFunc("POST /api/push", s.handlePush)
	mux.HandleFunc("POST /api/run", s.runCommand)
	addr := env("HEMMA_UI_ADDR", ":8080")
	log.Printf("hemma-ui listening on %s (repo %s)", addr, s.repo)
	log.Fatal(http.ListenAndServe(addr, guard(pw, mux)))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// guard enforces HTTP basic auth (username "admin") and, for mutating
// requests, a same-origin check so a page on another site cannot drive it.
// Responses are never cached: command output can include config and file paths.
func guard(pw string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(p), []byte(pw)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="hemma"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		}
		if strings.HasPrefix(r.URL.Path, "/static/fonts/") {
			w.Header().Set("Cache-Control", "private, max-age=86400") // fixed font files: no need to refetch on every load
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
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
			"auth-mode": string(v.Auth.Mode), "auth-groups": strings.Join(v.Auth.Groups, ","),
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
		hosts = append(hosts, map[string]any{"name": n, "ip": h.IP,
			"trusted-proxies": strings.Join(h.TrustedProxies, ",")})
	}
	d := cfg.Defaults
	values := map[string]string{
		"dns-host": d.DNSHost, "auth-snippet": d.AuthSnippet,
		"auth-service": d.AuthService, "local-networks": strings.Join(d.LocalNetworks, ","),
		"tunnel-dir": d.TunnelDir,
	}
	settings := []map[string]any{}
	for _, ds := range defaultSettings {
		settings = append(settings, map[string]any{"key": ds.Key, "label": ds.Label, "help": ds.Help,
			"kind": ds.Kind, "clearable": ds.Clearable, "value": values[ds.Key]})
	}
	writeJSON(w, 200, map[string]any{
		"services": svcs, "hosts": hosts, "domains": cfg.DomainNames(), "settings": settings, "version": s.version,
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
		err = s.checkState(c, in.Values)
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "output": err.Error()})
		return
	}
	if !c.ReadOnly {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if gated(c) {
		if ch := s.changes(); len(ch.Files) > 0 {
			writeJSON(w, 409, map[string]any{"ok": false, "dirty": true,
				"output": "Refused: there are uncommitted changes. Commit them with a change message first."})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, append([]string{"-C", s.repo}, argv...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	// Log the command key only: arguments and output can carry secrets.
	log.Printf("run %q -> err=%v", c.Key, err)
	writeJSON(w, 200, map[string]any{"ok": err == nil, "output": string(out)})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
