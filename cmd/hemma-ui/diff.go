package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"hemma/internal/config"
)

// A readable "what changed" for the commit dialog: services.yaml as last committed versus now,
// compared as things (services, hosts, domains, defaults) rather than as text. Everything hemma
// derives from it (the generated files) is only summarised — nobody reviews those by hand.

type fieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type entityChange struct {
	Kind   string        `json:"kind"`   // Service | Host | Domain | Setting
	Name   string        `json:"name"`   // e.g. "demo"
	Status string        `json:"status"` // added | removed | changed
	Fields []fieldChange `json:"fields"`
}

type diffResult struct {
	Entities  []entityChange `json:"entities"`
	Generated []string       `json:"generated"` // files hemma regenerated from the above
	Other     []change       `json:"other"`     // hand-edited files: no meaningful summary, listed by name
	// Reformatted: services.yaml differs on disk but no value in it changed. Hemma rewrites the file
	// wholesale on every edit, so setting something and clearing it again leaves different whitespace.
	Reformatted bool `json:"reformatted"`
}

const none = "—"

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return none
	}
	return s
}
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

type kv struct{ k, v string }

func serviceKV(sv config.Service) []kv {
	status := "enabled"
	if sv.Disabled {
		status = "disabled"
	}
	mode := string(sv.Auth.Mode)
	if mode == "" {
		mode = "none"
	}
	return []kv{
		{"Address", sv.FQDN}, {"Host", sv.Host}, {"Backend", sv.Backend}, {"Status", status},
		{"Auth mode", mode}, {"Auth groups", strings.Join(sv.Auth.Groups, ", ")},
		{"Bypass paths", strings.Join(sv.Auth.BypassPaths, ", ")},
		{"Policy: local", string(sv.Auth.Policy.Local)}, {"Policy: public", string(sv.Auth.Policy.Public)},
		{"Reachable from the internet", yesNo(sv.Tunnel)},
	}
}

func hostKV(h config.Host) []kv {
	return []kv{{"IP address", h.IP}, {"Extra trusted proxies", strings.Join(h.TrustedProxies, ", ")}}
}

func settingKVs(d config.Defaults) map[string]string {
	return map[string]string{
		"dns-host": d.DNSHost, "auth-service": d.AuthService, "auth-snippet": d.AuthSnippet,
		"local-networks": strings.Join(d.LocalNetworks, ", "), "tunnel-dir": d.TunnelDir,
	}
}

// compare turns two ordered field lists into the fields that differ (all of them for added/removed).
func compare(before, after []kv, status string) []fieldChange {
	var out []fieldChange
	for i := range after {
		var b, a string
		if before != nil {
			b = before[i].v
		}
		a = after[i].v
		if status == "changed" && b == a {
			continue
		}
		if status == "removed" {
			a = ""
		}
		out = append(out, fieldChange{Field: after[i].k, Before: orNone(b), After: orNone(a)})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// headConfig is services.yaml as of the last commit (an empty config if there is none yet).
func (s *server) headConfig() *config.Config {
	dir, err := os.MkdirTemp("", "hemma-ui-head")
	if err != nil {
		cfg, _ := config.Load(filepath.Join(os.TempDir(), "hemma-ui-no-such-dir", "services.yaml"))
		return cfg
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "services.yaml")
	if out, err := s.git(10*time.Second, "show", "HEAD:services.yaml"); err == nil {
		_ = os.WriteFile(p, []byte(out), 0o600)
	}
	cfg, _ := config.Load(p) // absent or unreadable → empty config, i.e. everything shows as added
	return cfg
}

func (s *server) diff() (diffResult, error) {
	res := diffResult{Entities: []entityChange{}, Generated: []string{}, Other: []change{}}
	now, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		return res, err
	}
	old := s.headConfig()

	add := func(kind, name, status string, before, after []kv) {
		f := compare(before, after, status)
		if status == "changed" && len(f) == 0 {
			return
		}
		res.Entities = append(res.Entities, entityChange{Kind: kind, Name: name, Status: status, Fields: f})
	}
	for _, n := range sortedKeys(now.Services) {
		if o, ok := old.Services[n]; ok {
			add("Service", n, "changed", serviceKV(o), serviceKV(now.Services[n]))
		} else {
			add("Service", n, "added", nil, serviceKV(now.Services[n]))
		}
	}
	for _, n := range sortedKeys(old.Services) {
		if _, ok := now.Services[n]; !ok {
			add("Service", n, "removed", serviceKV(old.Services[n]), serviceKV(old.Services[n]))
		}
	}
	for _, n := range sortedKeys(now.Hosts) {
		if o, ok := old.Hosts[n]; ok {
			add("Host", n, "changed", hostKV(o), hostKV(now.Hosts[n]))
		} else {
			add("Host", n, "added", nil, hostKV(now.Hosts[n]))
		}
	}
	for _, n := range sortedKeys(old.Hosts) {
		if _, ok := now.Hosts[n]; !ok {
			add("Host", n, "removed", hostKV(old.Hosts[n]), hostKV(old.Hosts[n]))
		}
	}
	for _, n := range sortedKeys(now.Domains) {
		if _, ok := old.Domains[n]; !ok {
			res.Entities = append(res.Entities, entityChange{Kind: "Domain", Name: n, Status: "added", Fields: []fieldChange{}})
		}
	}
	for _, n := range sortedKeys(old.Domains) {
		if _, ok := now.Domains[n]; !ok {
			res.Entities = append(res.Entities, entityChange{Kind: "Domain", Name: n, Status: "removed", Fields: []fieldChange{}})
		}
	}
	ov, nv := settingKVs(old.Defaults), settingKVs(now.Defaults)
	for _, d := range defaultSettings {
		if ov[d.Key] != nv[d.Key] {
			res.Entities = append(res.Entities, entityChange{Kind: "Setting", Name: d.Label, Status: "changed",
				Fields: []fieldChange{{Field: d.Label, Before: orNone(ov[d.Key]), After: orNone(nv[d.Key])}}})
		}
	}

	yamlChanged := false
	for _, f := range s.changes().Files {
		switch {
		case f.Path == "services.yaml":
			yamlChanged = true
		case s.isGeneratedChange(f):
			res.Generated = append(res.Generated, f.Path)
		default:
			res.Other = append(res.Other, f)
		}
	}
	res.Reformatted = yamlChanged && len(res.Entities) == 0
	return res, nil
}

// isGeneratedChange: is this changed file one of hemma's outputs? A present file carries a header;
// a deleted one can no longer be read, so it is recognised by where hemma writes things.
func (s *server) isGeneratedChange(f change) bool {
	if f.Path == "hemma-manifest.yaml" {
		return true
	}
	if _, err := os.Stat(filepath.Join(s.repo, f.Path)); err == nil {
		return isGenerated(filepath.Join(s.repo, f.Path))
	}
	for _, m := range []string{"/sites/", "/tls/", "/dnsmasq.d/", ".generated.", "cloudflared/config.yml"} {
		if strings.Contains(f.Path, m) {
			return true
		}
	}
	return false
}

// handleDiscard throws away the unsaved changes hemma itself made: services.yaml, the manifest and
// the generated files go back to the last commit. Files hemma doesn't manage (a snippet you wrote,
// a note) are never touched — they are your work, not hemma's output.
func (s *server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var restore []string
	removed := 0
	for _, f := range s.changes().Files {
		if f.Path != "services.yaml" && !s.isGeneratedChange(f) {
			continue
		}
		if strings.HasPrefix(f.Path, `"`) { // git quotes unusual names; leave those alone
			continue
		}
		if f.Status == "??" { // a new generated file: nothing to restore it to, so remove it
			if real, err := s.insideRepo(f.Path); err == nil && os.Remove(real) == nil {
				removed++
			}
			continue
		}
		restore = append(restore, f.Path)
	}
	if len(restore) > 0 {
		if out, err := s.git(time.Minute, append([]string{"checkout", "HEAD", "--"}, restore...)...); err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "output": out})
			return
		}
	}
	log.Printf("discard: restored %d, removed %d", len(restore), removed)
	writeJSON(w, 200, map[string]any{"ok": true, "output": "Discarded", "restored": len(restore), "removed": removed})
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	d, err := s.diff()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, d)
}
