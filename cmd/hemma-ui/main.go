// hemma-ui is a minimal single-host web UI for services.yaml: view, edit and
// apply. It never writes generated files or services.yaml itself — every
// mutation and the apply step shell out to the hemma binary, so validation and
// the single-writer invariant (design §6) stay exactly as on the CLI.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"hemma/internal/config"
)

//go:embed static/index.html
var static embed.FS

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type server struct {
	repo, bin string
	mu        sync.Mutex // one hemma invocation at a time
}

type svcJSON struct {
	Name        string   `json:"name"`
	FQDN        string   `json:"fqdn"`
	Host        string   `json:"host"`
	Backend     string   `json:"backend"`
	Disabled    bool     `json:"disabled"`
	AuthMode    string   `json:"auth_mode"`
	AuthGroups  []string `json:"auth_groups"`
	BypassPaths []string `json:"bypass_paths"`
	PolicyLocal string   `json:"policy_local"`
	PolicyPub   string   `json:"policy_public"`
	Tunnel      bool     `json:"tunnel"`
}

func main() {
	pw := os.Getenv("HEMMA_UI_PASSWORD")
	if pw == "" {
		log.Fatal("HEMMA_UI_PASSWORD must be set — refusing to start without a login")
	}
	s := &server{repo: env("HEMMA_REPO", "/repo"), bin: env("HEMMA_BIN", "hemma")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := static.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("PUT /api/services/{name}", s.save)
	mux.HandleFunc("DELETE /api/services/{name}", s.remove)
	mux.HandleFunc("POST /api/services/{name}/{action}", s.toggle)
	mux.HandleFunc("POST /api/apply", s.apply)
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
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	svcs := []svcJSON{}
	for _, n := range names {
		v := cfg.Services[n]
		svcs = append(svcs, svcJSON{
			Name: n, FQDN: v.FQDN, Host: v.Host, Backend: v.Backend, Disabled: v.Disabled,
			AuthMode: string(v.Auth.Mode), AuthGroups: v.Auth.Groups, BypassPaths: v.Auth.BypassPaths,
			PolicyLocal: string(v.Auth.Policy.Local), PolicyPub: string(v.Auth.Policy.Public), Tunnel: v.Tunnel,
		})
	}
	hosts := map[string]string{}
	for n, h := range cfg.Hosts {
		hosts[n] = h.IP
	}
	writeJSON(w, 200, map[string]any{
		"services": svcs, "hosts": hosts, "domains": cfg.DomainNames(),
		"defaults": cfg.Defaults,
	})
}

// save adds the service if new, else updates it. Every field is sent
// explicitly so an edit is a full replace of what the form shows.
func (s *server) save(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in svcJSON
	if !nameRe.MatchString(name) || json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid service name or body"})
		return
	}
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	_, exists := cfg.Services[name]
	mode := in.AuthMode
	if mode == "" {
		mode = "none"
	}
	args := []string{"update", "service", name}
	if !exists {
		args = []string{"add", "service", name}
	}
	args = append(args, "--fqdn", in.FQDN, "--host", in.Host, "--backend", in.Backend,
		"--auth-mode", mode, "--tunnel="+fmt.Sprint(in.Tunnel))
	if len(in.AuthGroups) > 0 {
		args = append(args, "--auth-groups", strings.Join(in.AuthGroups, ","))
	}
	if exists || len(in.BypassPaths) > 0 {
		args = append(args, "--bypass-paths", strings.Join(in.BypassPaths, ","))
	}
	if exists || in.PolicyLocal != "" {
		args = append(args, "--auth-policy-local", in.PolicyLocal)
	}
	if exists || in.PolicyPub != "" {
		args = append(args, "--auth-policy-public", in.PolicyPub)
	}
	s.run(w, 2*time.Minute, args...)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeJSON(w, 400, map[string]any{"error": "invalid service name"})
		return
	}
	s.run(w, time.Minute, "remove", "service", name)
}

func (s *server) toggle(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if !nameRe.MatchString(name) || (action != "enable" && action != "disable") {
		writeJSON(w, 400, map[string]any{"error": "invalid request"})
		return
	}
	s.run(w, time.Minute, action, "service", name)
}

func (s *server) apply(w http.ResponseWriter, r *http.Request) {
	s.run(w, 5*time.Minute, "apply")
}

// run executes `hemma -C <repo> <args...>` and returns its combined output.
// args are passed as an argv vector, never through a shell.
func (s *server) run(w http.ResponseWriter, timeout time.Duration, args ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, append([]string{"-C", s.repo}, args...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	log.Printf("hemma %s -> err=%v", strings.Join(args, " "), err)
	writeJSON(w, 200, map[string]any{"ok": err == nil, "output": string(out)})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
