package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	cases := []struct {
		key  string
		vals map[string]any
		want []string
		err  string
	}{
		{"add host", map[string]any{"name": "pi", "ip": "192.0.2.1", "trusted-proxies": "192.0.2.9"},
			[]string{"add", "host", "pi", "192.0.2.1", "--trusted-proxies=192.0.2.9"}, ""},
		// empty optional flags are dropped on add, kept (clearing) on update
		{"add host", map[string]any{"name": "pi", "ip": "192.0.2.1", "trusted-proxies": ""}, []string{"add", "host", "pi", "192.0.2.1"}, ""},
		{"update host", map[string]any{"name": "pi", "ip": "192.0.2.2", "trusted-proxies": ""},
			[]string{"update", "host", "pi", "--ip=192.0.2.2", "--trusted-proxies="}, ""},
		// flags-first commands
		{"verify", map[string]any{"all": true, "fqdn": "a.example.com"}, []string{"verify", "--all=true", "a.example.com"}, ""},
		{"defaults set", map[string]any{"key": "tunnel-dir", "value": "-"}, []string{"defaults", "set", "tunnel-dir", "-"}, ""},
		{"measure", map[string]any{"target": "https://a.example.com", "runs": float64(3)}, []string{"measure", "--runs=3", "https://a.example.com"}, ""},
		// rejections
		{"add host", map[string]any{"name": "--evil", "ip": "1.2.3.4"}, nil, "invalid"},
		{"add host", map[string]any{"name": "a b", "ip": "1.2.3.4"}, nil, "invalid"},
		{"add host", map[string]any{"name": "pi"}, nil, "required"},
		{"defaults set", map[string]any{"key": "shell", "value": "x"}, nil, "not an allowed"},
		{"add domain", map[string]any{"name": "-"}, nil, "invalid"},
		{"measure", map[string]any{"target": "t", "runs": float64(99999)}, nil, "0-1000"},
		{"add service", map[string]any{"name": "s", "fqdn": "a", "host": "h", "backend": "b", "auth-mode": "root"}, nil, "not an allowed"},
		{"add host", map[string]any{"name": "pi", "ip": "1.2.3.4", "trusted-proxies": "x\n--evil"}, nil, "invalid"}}
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

// The UI is single-host for now: no ssh targets or deploy-host are exposed.
// `deploy` itself stays: with one host it pulls and applies locally, no ssh.
func TestSingleHostOnly(t *testing.T) {
	d := lookup("deploy")
	if d == nil || len(d.Fields) != 0 {
		t.Error("deploy must exist and take no host arguments")
	}
	for _, c := range commands {
		for _, f := range c.Fields {
			if f.Name == "ssh" {
				t.Errorf("%s exposes ssh", c.Key)
			}
			if c.Key == "defaults set" {
				for _, o := range f.Options {
					if o == "deploy-host" {
						t.Error("deploy-host must not be settable")
					}
				}
			}
		}
	}
}

// Credential minting prints secrets the browser can't act on: it stays CLI-only.
func TestNoCredentialMinting(t *testing.T) {
	for _, k := range []string{"create app oidc", "create user"} {
		if lookup(k) != nil {
			t.Errorf("%q must not be exposed", k)
		}
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
