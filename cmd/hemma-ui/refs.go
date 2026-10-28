package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"hemma/internal/config"
	"hemma/internal/plan"
)

// Typed fields. A field with a Ref is not "a string": it is a host, a service, a
// CIDR list… The browser renders the matching picker (so only valid choices are
// offered), and the server re-checks the value — the browser is a convenience,
// not the control. Two kinds of check:
//   - stateless (shape): validateRef, run while building the argv;
//   - stateful (must exist in services.yaml): checkState, run just before executing.

var (
	svcNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	backendRe = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]+$`) // same shape hemma's plan enforces
	dnsNameRe = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$`)
	pathRe    = regexp.MustCompile(`^/[^\s,]*$`)
	relPathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
	// An ssh(1) destination: [user@]host. It must not start with '-' (deploy runs `ssh <dest>`, so a leading dash would be read as an
	// option) and has no spaces or '=' — nothing that could carry an option or a command along.
	sshDestRe = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9._-]*@)?[A-Za-z0-9][A-Za-z0-9.:_-]*$`)
)

// stateful refs need the loaded config; everything else is shape-only.
var statefulRefs = map[string]bool{"host": true, "service": true, "domain": true, "fqdn": true, "service-fqdn": true}

// validateRef checks the shape of one value (list fields are split first).
func validateRef(ref, v string) error {
	switch ref {
	case "svcname", "name":
		if !svcNameRe.MatchString(v) {
			return fmt.Errorf("%q is not a valid name (letters, digits, '.', '_', '-')", v)
		}
	case "backend":
		if !backendRe.MatchString(v) {
			return fmt.Errorf("%q is not name:port", v)
		}
		if p, _ := strconv.Atoi(v[strings.LastIndex(v, ":")+1:]); p < 1 || p > 65535 {
			return fmt.Errorf("%q: port must be 1-65535", v)
		}
	case "dnsname":
		if !dnsNameRe.MatchString(v) {
			return fmt.Errorf("%q is not a valid domain name", v)
		}
	case "ip":
		if net.ParseIP(v) == nil {
			return fmt.Errorf("%q is not an IP address", v)
		}
	case "cidr":
		if _, _, err := net.ParseCIDR(v); err != nil {
			return fmt.Errorf("%q is not a CIDR range (e.g. 192.168.1.0/24)", v)
		}
	case "ipcidr":
		if net.ParseIP(v) == nil {
			if _, _, err := net.ParseCIDR(v); err != nil {
				return fmt.Errorf("%q is not an IP address or CIDR range", v)
			}
		}
	case "path":
		if !pathRe.MatchString(v) {
			return fmt.Errorf("%q is not a path starting with '/'", v)
		}
	case "ssh":
		if !sshDestRe.MatchString(v) {
			return fmt.Errorf("%q is not an ssh destination (host or user@host)", v)
		}
	case "relpath":
		if !relPathRe.MatchString(v) || strings.Contains(v, "..") || strings.HasPrefix(v, "/") || hasDotSegment(v) {
			return fmt.Errorf("%q is not a path inside the repo", v)
		}
	}
	return nil
}

// checkAuthFields refuses an auth combination hemma would persist and then skip (design: plan.go's
// groups / bypass_paths / policy rules): groups and bypass paths only mean something when the mode
// goes through the auth provider (forward|oidc), and so is a per-horizon policy (oidc has no bypass level).
// The form never offers them otherwise; this is the server not trusting the form. No state needed.
func checkAuthFields(c *command, vals map[string]any) error {
	if c.Key != "add service" && c.Key != "update service" {
		return nil
	}
	str := func(k string) string { v, _ := vals[k].(string); return strings.TrimSpace(v) }
	mode := str("auth-mode")
	usesProvider := mode == "forward" || mode == "oidc"
	if !usesProvider && str("auth-groups") != "" {
		return fmt.Errorf("Auth groups need an auth mode of forward or oidc — pick one, or clear the groups")
	}
	if !usesProvider && str("bypass-paths") != "" {
		return fmt.Errorf("Bypass paths need an auth mode of forward or oidc — pick one, or clear them")
	}
	if !usesProvider && (str("auth-policy-local") != "" || str("auth-policy-public") != "") {
		return fmt.Errorf("A per-horizon policy needs an auth mode of forward or oidc — pick one, or clear the policy")
	}
	if usesProvider {
		tunnel, _ := vals["tunnel"].(bool)
		pol := config.AuthPolicy{Local: config.PolicyLevel(str("auth-policy-local")), Public: config.PolicyLevel(str("auth-policy-public"))}.ForService(tunnel)
		if pol.PublicWeakerThanLocal() {
			return fmt.Errorf("Public (%s) is weaker than local (%s) — a request from outside must not get an easier gate than one from the LAN", pol.ResolvePublic(), pol.ResolveLocal())
		}
	}
	if mode == "oidc" && (str("auth-policy-local") == "bypass" || str("auth-policy-public") == "bypass") {
		return fmt.Errorf("An oidc login is always authenticated, so a policy of bypass does not apply — use one_factor, two_factor or deny")
	}
	return nil
}

// effectiveValues is what checkAuthFields should judge: for an `update service` the service AS IT WOULD END UP. The kernel
// overrides only the flags that were given, so a request may carry just the one thing it changes (a role toggled on the Access
// page sends only `auth-groups`); a key that is absent takes the service's current value, one that is present — even empty,
// which clears — wins, exactly as the kernel applies it. Anything else (another command, an unknown service, a file that
// does not load) is returned as it came: checkState reports the missing service, and the check stays as strict as it was.
func (s *server) effectiveValues(c *command, vals map[string]any) map[string]any {
	if c.Key != "update service" {
		return vals
	}
	name, _ := vals["name"].(string)
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		return vals
	}
	svc, ok := cfg.Services[strings.TrimSpace(name)]
	if !ok {
		return vals
	}
	out := map[string]any{
		"auth-mode": string(svc.Auth.Mode), "auth-groups": strings.Join(svc.Auth.Groups, ","),
		"bypass-paths":       strings.Join(svc.Auth.BypassPaths, ","),
		"auth-policy-local":  string(svc.Auth.Policy.Local),
		"auth-policy-public": string(svc.Auth.Policy.Public), "tunnel": svc.Tunnel,
	}
	for k, v := range vals {
		out[k] = v
	}
	return out
}

// checkState verifies every state-dependent value of a command against services.yaml.
func (s *server) checkState(c *command, vals map[string]any) error {
	need := false
	for _, f := range c.Fields {
		if statefulRefs[f.Ref] {
			need = true
		}
	}
	if !need && c.Key != "defaults set" {
		return nil
	}
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		return err
	}
	for _, f := range c.Fields {
		v, _ := vals[f.Name].(string)
		v = strings.TrimSpace(v)
		if v == "" || !statefulRefs[f.Ref] {
			continue
		}
		switch f.Ref {
		case "host":
			if _, ok := cfg.Hosts[v]; !ok {
				return fmt.Errorf("%s: there is no host %q", f.Label, v)
			}
		case "service":
			if _, ok := cfg.Services[v]; !ok {
				return fmt.Errorf("%s: there is no service %q", f.Label, v)
			}
		case "domain":
			if _, ok := cfg.Domains[v]; !ok {
				return fmt.Errorf("%s: there is no domain %q", f.Label, v)
			}
		case "fqdn":
			// The planner's own shape rule, first: an address it would skip is refused here instead of being
			// persisted and reported as "saved, with problems".
			if !plan.ValidFQDN(v) {
				return fmt.Errorf("%s: %q is not a valid address (letters, digits and hyphens, dot-separated, ending in a domain)", f.Label, v)
			}
			if _, ok := cfg.MatchDomain(v); !ok {
				return fmt.Errorf("%s: %q is not under a defined domain", f.Label, v)
			}
		case "service-fqdn":
			found := false
			for _, sv := range cfg.Services {
				found = found || sv.FQDN == v
			}
			if !found {
				return fmt.Errorf("%s: no service has the address %q", f.Label, v)
			}
		}
	}
	if c.Key == "defaults set" {
		k, _ := vals["key"].(string)
		v, _ := vals["value"].(string)
		return s.checkDefault(cfg, k, strings.TrimSpace(v))
	}
	return nil
}

// defaultSetting describes one repo-wide default so the UI can offer the right editor.
type defaultSetting struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Help      string `json:"help"`
	Kind      string `json:"kind"` // host | service | cidrs | file | relpath
	Clearable bool   `json:"clearable"`
}

var defaultSettings = []defaultSetting{
	{"dns-host", "DNS host", "The host that runs the resolver (Pi-hole). Every service's DNS record is served from it.", "host", false},
	{"deploy-host", "Deploy host", "The one host that Deploy runs from, and that this page must run on. Deploy refuses to run anywhere else.", "host", true},
	{"auth-service", "Auth service", "The service that is the forward-auth backend (e.g. Authelia). It is exempt from its own gate to avoid a redirect loop.", "service", true},
	{"auth-snippet", "Auth snippet", "A Caddy file in the repo whose contents become the shared (auth) block. Without it, protected services are not actually gated.", "file", true},
	{"local-networks", "Local networks", "The ranges that count as 'at home' for services whose auth rules differ between local and public requests.", "cidrs", true},
	{"tunnel-dir", "Tunnel directory", "The folder (inside each host's directory) where the cloudflared config is written. Services with the internet tunnel enabled are unreachable until it is set.", "relpath", true},
}

func settingByKey(k string) *defaultSetting {
	for i := range defaultSettings {
		if defaultSettings[i].Key == k {
			return &defaultSettings[i]
		}
	}
	return nil
}

// checkDefault validates a `defaults set` value by the setting's type.
func (s *server) checkDefault(cfg *config.Config, key, v string) error {
	d := settingByKey(key)
	if d == nil {
		return fmt.Errorf("%q is not a setting you can change here", key)
	}
	if v == "-" {
		if !d.Clearable {
			return fmt.Errorf("%s cannot be cleared", d.Label)
		}
		return nil
	}
	switch d.Kind {
	case "host":
		if _, ok := cfg.Hosts[v]; !ok {
			return fmt.Errorf("there is no host %q", v)
		}
	case "service":
		if _, ok := cfg.Services[v]; !ok {
			return fmt.Errorf("there is no service %q", v)
		}
	case "cidrs":
		for _, p := range strings.Split(v, ",") {
			if err := validateRef("cidr", strings.TrimSpace(p)); err != nil {
				return err
			}
		}
	case "file", "relpath":
		if err := validateRef("relpath", v); err != nil {
			return err
		}
		if d.Kind == "file" {
			real, err := s.insideRepo(v)
			if err != nil {
				return err
			}
			if st, err := os.Stat(real); err != nil || st.IsDir() {
				return fmt.Errorf("there is no file %q in the repo", v)
			}
			if isGenerated(real) {
				return fmt.Errorf("%q is generated by hemma — choose a hand-written file", v)
			}
		}
	}
	return nil
}

// ---- file browser: the repo as mounted into the container ----
// The UI can only ever see what is under the repo root. Every path is resolved and checked to
// stay inside it (".." and symlinks that leave the repo are refused), dotfiles and .git are hidden,
// and symlinks are not followed or listed.

// hasDotSegment reports whether any path component is hidden (".git", ".env", …). ".." is not a
// component here: it is neutralised separately by insideRepo's rooted Clean.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

// insideRepo resolves a repo-relative path to a real path that is guaranteed to lie inside the repo.
func (s *server) insideRepo(rel string) (string, error) {
	root, err := filepath.EvalSymlinks(s.repo)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean("/" + rel)[1:] // rooted Clean: ".." can't climb above the root
	real, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("there is no %q in the repo", rel)
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the repo", rel)
	}
	return real, nil
}

// isGenerated reports whether a file is one of hemma's own outputs (they carry a header).
func isGenerated(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := f.Read(head)
	return strings.Contains(string(head[:n]), "GENERATED by hemma")
}

type fileEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Dir       bool   `json:"dir"`
	Generated bool   `json:"generated,omitempty"`
}

// browse lists one directory of the repo: folders first, then files, each sorted by name.
func (s *server) browse(rel string) (string, []fileEntry, error) {
	if hasDotSegment(rel) {
		return "", nil, fmt.Errorf("hidden folders (such as .git) are not browsable")
	}
	real, err := s.insideRepo(rel)
	if err != nil {
		return "", nil, err
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return "", nil, fmt.Errorf("cannot read %q", rel)
	}
	clean := filepath.ToSlash(filepath.Clean("/" + rel)[1:])
	if clean == "." {
		clean = ""
	}
	out := []fileEntry{}
	for _, d := range des {
		name := d.Name()
		if strings.HasPrefix(name, ".") || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		e := fileEntry{Name: name, Path: strings.TrimPrefix(clean+"/"+name, "/"), Dir: d.IsDir()}
		if !e.Dir && strings.HasSuffix(name, ".caddy") {
			e.Generated = isGenerated(filepath.Join(real, name))
		}
		if out = append(out, e); len(out) >= 1000 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return clean, out, nil
}

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	path, entries, err := s.browse(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "entries": entries})
}
