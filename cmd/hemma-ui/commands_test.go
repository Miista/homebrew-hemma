package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"hemma/internal/config"
)

func TestBuild(t *testing.T) {
	cases := []struct {
		key  string
		vals map[string]any
		want []string
		err  string
	}{
		{"add host", map[string]any{"name": "edge", "ip": "192.0.2.1", "trusted-proxies": "192.0.2.9"},
			[]string{"add", "host", "edge", "192.0.2.1", "--trusted-proxies=192.0.2.9"}, ""},
		// empty optional flags are dropped on add, kept (clearing) on update
		{"add host", map[string]any{"name": "edge", "ip": "192.0.2.1", "trusted-proxies": ""}, []string{"add", "host", "edge", "192.0.2.1"}, ""},
		{"update host", map[string]any{"name": "edge", "ip": "192.0.2.2", "trusted-proxies": ""},
			[]string{"update", "host", "edge", "--ip=192.0.2.2", "--trusted-proxies="}, ""},
		// an ssh destination is [user@]host; empty clears it on update, is dropped on add
		{"add host", map[string]any{"name": "edge", "ip": "192.0.2.1", "ssh": "admin@192.0.2.1"},
			[]string{"add", "host", "edge", "192.0.2.1", "--ssh=admin@192.0.2.1"}, ""},
		{"add host", map[string]any{"name": "edge", "ip": "192.0.2.1", "ssh": ""}, []string{"add", "host", "edge", "192.0.2.1"}, ""},
		{"update host", map[string]any{"name": "edge", "ssh": ""}, []string{"update", "host", "edge", "--ssh="}, ""},
		// flags-first commands
		{"verify", map[string]any{"all": true, "fqdn": "a.example.com"}, []string{"verify", "--all=true", "a.example.com"}, ""},
		{"defaults set", map[string]any{"key": "tunnel-dir", "value": "-"}, []string{"defaults", "set", "tunnel-dir", "-"}, ""},
		{"measure", map[string]any{"target": "web", "runs": float64(3)}, []string{"measure", "--runs=3", "web"}, ""},
		// rejections
		{"add host", map[string]any{"name": "--evil", "ip": "1.2.3.4"}, nil, "not a valid name"},
		{"add host", map[string]any{"name": "a b", "ip": "1.2.3.4"}, nil, "not a valid name"},
		{"add host", map[string]any{"name": "edge", "ip": "999.1.1.1"}, nil, "not an IP address"},
		{"add host", map[string]any{"name": "edge", "ip": "1.2.3.4", "trusted-proxies": "10.0.0.0/8,nope"}, nil, "not an IP address or CIDR"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "web:99999"}, nil, "port must be"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "http://web:80"}, nil, "not name:port"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "b:1", "bypass-paths": "/ok,nope"}, nil, "not a path"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "b:1", "auth-groups": "a,,b"}, nil, "not a valid name"},
		{"add host", map[string]any{"name": "edge"}, nil, "required"},
		{"defaults set", map[string]any{"key": "shell", "value": "x"}, nil, "not an allowed"},
		{"add domain", map[string]any{"name": "-"}, nil, "not a valid domain"},
		{"measure", map[string]any{"target": "t", "runs": float64(99999)}, nil, "0-1000"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "b:1", "auth-mode": "root"}, nil, "not an allowed"},
		{"add host", map[string]any{"name": "edge", "ip": "1.2.3.4", "trusted-proxies": "x\n--evil"}, nil, "not an IP address"},
		// ssh runs as `ssh <dest>`: nothing that reads as an option or carries a command
		{"update host", map[string]any{"name": "edge", "ssh": "-oProxyCommand=evil"}, nil, "not an ssh destination"},
		{"update host", map[string]any{"name": "edge", "ssh": "-v@host"}, nil, "not an ssh destination"},
		{"update host", map[string]any{"name": "edge", "ssh": "a b"}, nil, "not an ssh destination"},
		{"update host", map[string]any{"name": "edge", "ssh": "host;rm"}, nil, "not an ssh destination"}}
	for _, c := range cases {
		cmd := lookup(c.key)
		if cmd == nil {
			t.Fatalf("no command %q", c.key)
		}
		got, err := cmd.build(c.vals)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s %v: err = %v, want containing %q", c.key, c.vals, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: got %v, %v; want %v", c.key, c.vals, got, err, c.want)
		}
	}
}

// The UI deploys the whole fleet (the kernel's deploy ssh's to the other hosts), so a host's ssh destination and the
// deploy host are settable here — but `deploy` itself takes no arguments, and every ssh field is the validated "ssh" type
// (it runs as `ssh <dest>`, so a free-text field would be an option-injection hole).
func TestFleetSettings(t *testing.T) {
	d := lookup("deploy")
	if d == nil || len(d.Fields) != 0 {
		t.Error("deploy must exist and take no host arguments")
	}
	deployHost := false
	for _, c := range commands {
		for _, f := range c.Fields {
			if f.Name == "ssh" && f.Ref != "ssh" {
				t.Errorf("%s: the ssh field must be the validated ssh type, not %q", c.Key, f.Ref)
			}
			if c.Key == "defaults set" {
				for _, o := range f.Options {
					deployHost = deployHost || o == "deploy-host"
				}
			}
		}
	}
	if !deployHost {
		t.Error("deploy-host must be settable")
	}
}

// Credential minting prints secrets the browser can't act on: it stays CLI-only.
func TestNoCredentialMinting(t *testing.T) {
	for _, k := range []string{"create app oidc", "create user", "help", "completion", "version", "defaults"} {
		if lookup(k) != nil {
			t.Errorf("%q must not be exposed", k)
		}
	}
}

// The server re-checks what the browser pickers offer: a value that doesn't exist in
// services.yaml is refused even if someone bypasses the UI and posts it directly.
func TestCheckState(t *testing.T) {
	dir := t.TempDir()
	yaml := "hosts:\n    edge:\n        ip: 192.0.2.50\ndomains:\n    - example.com\ndefaults:\n    dns_host: edge\n" +
		"services:\n    web:\n        fqdn: web.example.com\n        host: edge\n        backend: web:80\n"
	if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snippet.caddy"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{repo: dir}
	cases := []struct {
		key  string
		vals map[string]any
		err  string // "" = must pass
	}{
		{"add service", map[string]any{"name": "n", "fqdn": "n.example.com", "host": "edge", "backend": "n:1"}, ""},
		{"add service", map[string]any{"name": "n", "fqdn": "n.example.com", "host": "ghost", "backend": "n:1"}, "no host"},
		{"add service", map[string]any{"name": "n", "fqdn": "n.other.org", "host": "edge", "backend": "n:1"}, "not under a defined domain"},
		// the planner's shape rule, checked before the file is touched: these would be persisted and then skipped
		{"add service", map[string]any{"name": "n", "fqdn": "foo bar.example.com", "host": "edge", "backend": "n:1"}, "not a valid address"},
		{"add service", map[string]any{"name": "n", "fqdn": "-x.example.com", "host": "edge", "backend": "n:1"}, "not a valid address"},
		{"add service", map[string]any{"name": "n", "fqdn": "x..example.com", "host": "edge", "backend": "n:1"}, "not a valid address"},
		{"update service", map[string]any{"name": "web", "fqdn": "under_score.example.com"}, "not a valid address"},
		{"remove service", map[string]any{"name": "web"}, ""},
		{"remove service", map[string]any{"name": "ghost"}, "no service"},
		{"remove domain", map[string]any{"name": "nope.com"}, "no domain"},
		{"verify", map[string]any{"fqdn": "web.example.com"}, ""},
		{"verify", map[string]any{"fqdn": "x.example.com"}, "no service has the address"},
		{"defaults set", map[string]any{"key": "dns-host", "value": "edge"}, ""},
		{"defaults set", map[string]any{"key": "dns-host", "value": "ghost"}, "no host"},
		{"defaults set", map[string]any{"key": "dns-host", "value": "-"}, "cannot be cleared"},
		{"defaults set", map[string]any{"key": "deploy-host", "value": "edge"}, ""},
		{"defaults set", map[string]any{"key": "deploy-host", "value": "ghost"}, "no host"},
		{"defaults set", map[string]any{"key": "deploy-host", "value": "-"}, ""},
		{"defaults set", map[string]any{"key": "auth-service", "value": "web"}, ""},
		{"defaults set", map[string]any{"key": "auth-service", "value": "ghost"}, "no service"},
		{"defaults set", map[string]any{"key": "auth-service", "value": "-"}, ""},
		{"defaults set", map[string]any{"key": "local-networks", "value": "10.0.10.0/24,192.168.0.0/16"}, ""},
		{"defaults set", map[string]any{"key": "local-networks", "value": "10.0.10.0"}, "not a CIDR"},
		{"defaults set", map[string]any{"key": "auth-snippet", "value": "snippet.caddy"}, ""},
		{"defaults set", map[string]any{"key": "auth-snippet", "value": "missing.caddy"}, "there is no"},
		{"defaults set", map[string]any{"key": "auth-snippet", "value": "../etc/passwd"}, "not a path inside the repo"},
		{"defaults set", map[string]any{"key": "tunnel-dir", "value": "cloudflared"}, ""},
		{"defaults set", map[string]any{"key": "tunnel-dir", "value": "/abs"}, "not a path inside the repo"},
	}
	for _, c := range cases {
		err := s.checkState(lookup(c.key), c.vals)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %v: err = %v, want %q", c.key, c.vals, err, c.err)
		}
	}
}

// The file browser only ever shows the repo, and never lets a path climb out of it.
func TestBrowse(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write := func(base, rel, body string) {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dir, "edge/caddy/data/auth.caddy", "# mine\n")
	write(dir, "edge/caddy/data/hemma.generated.caddy", "# GENERATED by hemma — do not edit\n")
	write(dir, "edge/caddy/data/Caddyfile", "import hemma.generated.caddy\n")
	write(dir, "zeta.txt", "x\n")
	write(dir, ".git/config", "secret\n")
	write(dir, ".hidden", "x\n")
	write(outside, "secret.caddy", "# not in the repo\n")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	s := &server{repo: dir}

	names := func(es []fileEntry) []string {
		var o []string
		for _, e := range es {
			o = append(o, e.Name)
		}
		return o
	}
	_, root, err := s.browse("")
	if err != nil || !reflect.DeepEqual(names(root), []string{"edge", "zeta.txt"}) {
		t.Errorf("root = %v, %v; want [edge zeta.txt] (folders first; .git, dotfiles and symlinks hidden)", names(root), err)
	}
	p, data, err := s.browse("edge/caddy/data")
	if err != nil || p != "edge/caddy/data" || !reflect.DeepEqual(names(data), []string{"Caddyfile", "auth.caddy", "hemma.generated.caddy"}) {
		t.Errorf("data = %q %v %v", p, names(data), err)
	}
	for _, e := range data {
		if e.Generated != (e.Name == "hemma.generated.caddy") {
			t.Errorf("%s: generated = %v", e.Name, e.Generated)
		}
	}
	if data[1].Path != "edge/caddy/data/auth.caddy" {
		t.Errorf("path = %q", data[1].Path)
	}
	// Anything that could leave the repo must be refused, or at worst be confined to it.
	for _, bad := range []string{"escape", "escape/secret.caddy", "edge/../../etc"} {
		if _, es, err := s.browse(bad); err == nil && len(es) > 0 && es[0].Name == "secret.caddy" {
			t.Errorf("browse(%q) leaked the outside directory", bad)
		}
	}
	if _, _, err := s.browse("escape"); err == nil {
		t.Error("a symlink leaving the repo must be refused")
	}
	for _, hidden := range []string{".git", ".git/hooks", "edge/.secret"} {
		if _, es, err := s.browse(hidden); err == nil || len(es) != 0 {
			t.Errorf("browse(%q) must refuse hidden folders, got %v %v", hidden, es, err)
		}
	}
	if err := s.checkDefault(&config.Config{}, "auth-snippet", ".git/config"); err == nil {
		t.Error("a file inside a hidden folder must not be choosable as the auth snippet")
	}
	if p, es, err := s.browse("../../.."); err != nil || p != "" || !reflect.DeepEqual(names(es), []string{"edge", "zeta.txt"}) {
		t.Errorf("'..' must clamp to the repo root, got %q %v %v", p, names(es), err)
	}
	if p, es, err := s.browse("/etc"); err == nil && len(es) > 0 && p != "etc" {
		t.Errorf("absolute path must be treated as repo-relative, got %q %v", p, names(es))
	}
	// Choosing a snippet: must exist, be inside the repo, and not be one of hemma's outputs.
	cfg := &config.Config{}
	for rel, want := range map[string]string{
		"edge/caddy/data/auth.caddy":            "",
		"edge/caddy/data/hemma.generated.caddy": "generated by hemma",
		"edge/caddy/data/nope.caddy":            "no ",
		"escape/secret.caddy":                   "outside the repo",
		"edge/caddy":                            "there is no file",
	} {
		err := s.checkDefault(cfg, "auth-snippet", rel)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("auth-snippet %q: err = %v, want %q", rel, err, want)
		}
	}
}

// The commit dialog shows what changed as things (services, hosts, defaults), not as file names.
func TestDiff(t *testing.T) {
	dir := t.TempDir()
	s := &server{repo: dir}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s.invalidateChanges() // the handlers do this after every change; this test edits the files itself
	}
	v1 := "hosts:\n    edge:\n        ip: 192.0.2.50\ndomains:\n    - example.com\ndefaults:\n    dns_host: edge\n" +
		"services:\n    demo:\n        fqdn: demo.example.com\n        host: edge\n        backend: demo:80\n" +
		"    web:\n        fqdn: web.example.com\n        host: edge\n        backend: web:80\n"
	write("services.yaml", v1)
	write("edge/caddy/data/sites/demo.caddy", "# GENERATED by hemma — do not edit\nold\n")
	write("hemma-manifest.yaml", "demo:\n    - edge/caddy/data/sites/demo.caddy\n") // what hemma wrote: this is what makes a file its own
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	// Clean tree: nothing to show.
	if d, err := s.diff(); err != nil || len(d.Entities) != 0 || len(d.Generated) != 0 || d.Reformatted {
		t.Fatalf("clean diff = %+v, %v", d, err)
	}
	// Only whitespace changed (hemma rewrites the file on every edit): no entity changes, but it is not "nothing".
	write("services.yaml", v1+"\n\n")
	if d, err := s.diff(); err != nil || len(d.Entities) != 0 || !d.Reformatted {
		t.Fatalf("format-only diff = %+v, %v (want no entities and Reformatted)", d, err)
	}
	write("services.yaml", v1)

	v2 := "hosts:\n    edge:\n        ip: 192.0.2.50\n        trusted_proxies:\n            - 10.0.0.0/8\ndomains:\n    - example.com\n    - example.net\n" +
		"defaults:\n    dns_host: edge\n    local_networks:\n        - 10.0.10.0/24\n" +
		"services:\n    demo:\n        fqdn: demo.example.com\n        host: edge\n        backend: demo:8080\n        tunnel: true\n" +
		"    docsapp:\n        fqdn: docs.example.com\n        host: edge\n        backend: docsapp:8000\n"
	write("services.yaml", v2)
	write("edge/caddy/data/sites/demo.caddy", "# GENERATED by hemma — do not edit\nnew\n")
	write("edge/caddy/data/sites/docsapp.caddy", "# GENERATED by hemma — do not edit\n")
	write("hemma-manifest.yaml", "demo:\n    - edge/caddy/data/sites/demo.caddy\ndocsapp:\n    - edge/caddy/data/sites/docsapp.caddy\n")
	write("notes.txt", "hand edited\n") // not in the manifest: somebody else's file
	write("edge/docker-compose.yml", "services: {}\n")

	d, err := s.diff()
	if err != nil {
		t.Fatal(err)
	}
	get := func(kind, name string) *entityChange {
		for i := range d.Entities {
			if d.Entities[i].Kind == kind && d.Entities[i].Name == name {
				return &d.Entities[i]
			}
		}
		return nil
	}
	field := func(e *entityChange, f string) *fieldChange {
		for i := range e.Fields {
			if e.Fields[i].Field == f {
				return &e.Fields[i]
			}
		}
		return nil
	}
	demo := get("Service", "demo")
	if demo == nil || demo.Status != "changed" {
		t.Fatalf("demo = %+v", demo)
	}
	if f := field(demo, "Backend"); f == nil || f.Before != "demo:80" || f.After != "demo:8080" {
		t.Errorf("backend change = %+v", f)
	}
	if f := field(demo, "Reachable from the internet"); f == nil || f.Before != "no" || f.After != "yes" {
		t.Errorf("tunnel change = %+v", f)
	}
	if field(demo, "Address") != nil || field(demo, "Host") != nil {
		t.Errorf("unchanged fields must not be listed: %+v", demo.Fields)
	}
	if p := get("Service", "docsapp"); p == nil || p.Status != "added" || field(p, "Address").Before != none || field(p, "Address").After != "docs.example.com" {
		t.Errorf("docsapp = %+v", p)
	}
	if w := get("Service", "web"); w == nil || w.Status != "removed" || field(w, "Backend").Before != "web:80" || field(w, "Backend").After != none {
		t.Errorf("web = %+v", w)
	}
	if h := get("Host", "edge"); h == nil || field(h, "Extra trusted proxies").After != "10.0.0.0/8" || field(h, "Extra trusted proxies").Before != none {
		t.Errorf("host = %+v", h)
	}
	if dm := get("Domain", "example.net"); dm == nil || dm.Status != "added" {
		t.Errorf("domain = %+v", dm)
	}
	if st := get("Setting", "Local networks"); st == nil || field(st, "Local networks").After != "10.0.10.0/24" {
		t.Errorf("setting = %+v", st)
	}
	sort.Strings(d.Generated)
	if !reflect.DeepEqual(d.Generated, []string{"edge/caddy/data/sites/demo.caddy", "edge/caddy/data/sites/docsapp.caddy", "hemma-manifest.yaml"}) {
		t.Errorf("generated = %v", d.Generated)
	}
	// Files that are not hemma's — a hand-edited note, a compose file — are not part of the change at all.
	for _, f := range s.changes().Files {
		if f.Path == "notes.txt" || f.Path == "edge/docker-compose.yml" {
			t.Errorf("%s is not hemma's and must not be listed as a change", f.Path)
		}
	}
}

// Discard reverts what hemma changed, and leaves everything that is the operator's own work alone.
func TestDiscard(t *testing.T) {
	dir := t.TempDir()
	s := &server{repo: dir}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(dir, rel)); return string(b) }
	const gen = "# GENERATED by hemma — do not edit\n"
	write("services.yaml", "hosts:\n    edge:\n        ip: 192.0.2.50\ndomains:\n    - example.com\nservices: {}\n")
	write("edge/caddy/data/sites/a.caddy", gen+"a-original\n")
	write("edge/caddy/data/sites/gone.caddy", gen+"gone-original\n")
	write("auth.caddy", "my snippet, original\n")
	write("hemma-manifest.yaml", "a:\n    - edge/caddy/data/sites/a.caddy\n    - edge/caddy/data/sites/gone.caddy\n")
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	write("services.yaml", "hosts:\n    edge:\n        ip: 192.0.2.99\ndomains:\n    - example.com\nservices: {}\n") // hemma edit
	write("edge/caddy/data/sites/a.caddy", gen+"a-changed\n")                                                        // regenerated
	write("edge/caddy/data/sites/new.caddy", gen+"new\n")                                                            // newly generated (untracked)
	write("hemma-manifest.yaml", "a:\n    - edge/caddy/data/sites/a.caddy\n    - edge/caddy/data/sites/gone.caddy\n    - edge/caddy/data/sites/new.caddy\n")
	if err := os.Remove(filepath.Join(dir, "edge/caddy/data/sites/gone.caddy")); err != nil { // generated, then deleted
		t.Fatal(err)
	}
	write("auth.caddy", "my snippet, EDITED BY HAND\n") // the operator's own work
	write("notes.txt", "hand written\n")                // untracked, not hemma's

	rec := httptest.NewRecorder()
	s.handleDiscard(rec, httptest.NewRequest("POST", "/api/discard", nil))
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("discard: %s", rec.Body.String())
	}
	if !strings.Contains(read("services.yaml"), "192.0.2.50") {
		t.Error("services.yaml must be restored to the last commit")
	}
	if read("edge/caddy/data/sites/a.caddy") != gen+"a-original\n" {
		t.Error("a regenerated file must be restored")
	}
	if read("edge/caddy/data/sites/gone.caddy") != gen+"gone-original\n" {
		t.Error("a deleted generated file must be restored")
	}
	if _, err := os.Stat(filepath.Join(dir, "edge/caddy/data/sites/new.caddy")); err == nil {
		t.Error("a newly generated file must be removed")
	}
	if read("auth.caddy") != "my snippet, EDITED BY HAND\n" || read("notes.txt") != "hand written\n" {
		t.Error("files hemma does not manage must be left exactly as they are")
	}
}

func TestRegistryUniqueKeys(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands {
		if seen[c.Key] {
			t.Errorf("duplicate command %q", c.Key)
		}
		seen[c.Key] = true
	}
}

// Save commits hemma's own files only: an unrelated file somebody is editing is neither listed nor committed with them.
func TestCommit_LeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	s := &server{repo: dir}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s.invalidateChanges()
	}
	write("services.yaml", "services: {}\n")
	write("edge/docker-compose.yml", "services: {}\n")
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	write("services.yaml", "hosts: {}\nservices: {}\n")
	write("edge/docker-compose.yml", "services: {x: {}}\n") // somebody's edit, tracked
	write("edge/restic/coverage-ignore", "*.tmp\n")         // somebody's new file, untracked

	rec := httptest.NewRecorder()
	s.handleCommit(rec, httptest.NewRequest("POST", "/api/commit", strings.NewReader(`{"message":"hemma change"}`)))
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("commit: %s", rec.Body.String())
	}
	if out, _ := s.git(time.Minute, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(out) != "services.yaml" {
		t.Errorf("only hemma's file may be committed, got %q", out)
	}
	out, _ := s.git(time.Minute, "status", "--porcelain")
	if !strings.Contains(out, "edge/docker-compose.yml") || !strings.Contains(out, "edge/restic/") {
		t.Errorf("the other files must still be uncommitted edits, status: %q", out)
	}
}

// A Save message is text: line breaks and tabs are fine, other control characters are refused before git sees them.
func TestCommit_RefusesControlCharactersInMessage(t *testing.T) {
	s := repoWithConfig(t)
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"message": "bad \x1f message"})
	s.handleCommit(rec, httptest.NewRequest("POST", "/api/commit", strings.NewReader(string(body))))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "control characters") {
		t.Errorf("a control character must be refused with 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// Save is all-or-nothing: when the commit fails after the add (a hook here), the add is undone, so no hemma file is
// left staged — and Discard, which only knows untracked and committed files, still works.
func TestCommit_FailedCommitLeavesNothingStaged(t *testing.T) {
	dir := t.TempDir()
	s := &server{repo: dir}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s.invalidateChanges()
	}
	write("services.yaml", "services: {}\n")
	write("hemma-manifest.yaml", "web:\n  - edge/sites/web.caddy\n")
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho no-commits-today\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("services.yaml", "hosts: {}\nservices: {}\n") // hemma's edit to a committed file
	write("edge/sites/web.caddy", "# generated\n")      // hemma's new generated file

	rec := httptest.NewRecorder()
	s.handleCommit(rec, httptest.NewRequest("POST", "/api/commit", strings.NewReader(`{"message":"hemma change"}`)))
	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "no-commits-today") {
		t.Fatalf("the hook must fail the save, with its output: %s", rec.Body.String())
	}
	if out, _ := s.git(time.Minute, "diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Errorf("nothing may be left staged after a failed commit, got %q", out)
	}
	status, _ := s.git(time.Minute, "status", "--porcelain", "--untracked-files=all")
	if !strings.Contains(status, " M services.yaml") || !strings.Contains(status, "?? edge/sites/web.caddy") {
		t.Errorf("the edits must be intact and the new file untracked again, status: %q", status)
	}
	// Discard still works: the new file is removed, the edit put back.
	s.invalidateChanges()
	rec = httptest.NewRecorder()
	s.handleDiscard(rec, httptest.NewRequest("POST", "/api/discard", nil))
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("discard: %s", rec.Body.String())
	}
	if out, _ := s.git(time.Minute, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("after discard the tree must be clean, status: %q", out)
	}
}

// "Ahead of origin" only means something to deploy when a commit touches a file hemma owns: a commit to a compose file is
// ahead too, but there is nothing for hemma to make live.
func TestChanges_AheadHemmaOnlyForOwnedFiles(t *testing.T) {
	root := t.TempDir()
	origin, dir := filepath.Join(root, "origin.git"), filepath.Join(root, "repo")
	s := &server{repo: dir}
	git := func(args ...string) {
		t.Helper()
		if out, err := s.git(time.Minute, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Skipf("git not available: %v %s", err, out)
	}
	if out, err := exec.Command("git", "clone", "-q", origin, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("services.yaml", "services: {}\n")
	write("edge/docker-compose.yml", "services: {}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	git("push", "-q", "-u", "origin", "main")

	if c := s.changes(); !c.HasRemote || c.Ahead != 0 || c.AheadHemma {
		t.Fatalf("level with origin: %+v", c)
	}
	// a commit to somebody else's file: ahead, but nothing for hemma to deploy
	write("edge/docker-compose.yml", "services: {x: {}}\n")
	git("commit", "-q", "-am", "edge/homepage: follow the moving tag")
	if c := s.changes(); c.Ahead != 1 || c.AheadHemma {
		t.Fatalf("a compose-only commit must not count as something to deploy: %+v", c)
	}
	// a commit to hemma's own file: now there is
	write("services.yaml", "hosts: {}\nservices: {}\n")
	git("commit", "-q", "-am", "add a host")
	if c := s.changes(); c.Ahead != 2 || !c.AheadHemma {
		t.Fatalf("a commit to services.yaml is something to deploy: %+v", c)
	}
}
