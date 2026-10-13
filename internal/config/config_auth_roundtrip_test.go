package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Whatever hemma WRITES it must be able to READ BACK. The none mode is the empty string internally and
// was written as `mode: ""`, which the reader refused: a service with mode none plus bypass paths, groups
// or a policy turned services.yaml into a file nothing (CLI or UI) could load again.
func TestAuth_EveryCombinationRoundTrips(t *testing.T) {
	modes := []AuthMode{AuthNone, AuthForward, AuthOIDC, AuthExternal}
	groups := [][]string{nil, {"admins"}, {"admins", "media"}}
	bypass := [][]string{nil, {"/api"}, {"/api", "/ping"}}
	policies := []AuthPolicy{{}, {Local: "bypass"}, {Local: "one_factor", Public: "two_factor"}}
	for _, m := range modes {
		for _, g := range groups {
			for _, b := range bypass {
				for _, p := range policies {
					in := Auth{Mode: m, Groups: g, BypassPaths: b, Policy: p}
					out, err := yaml.Marshal(map[string]any{"auth": in})
					if err != nil {
						t.Fatalf("marshal %+v: %v", in, err)
					}
					// The writer must spell the mode out. The reader tolerates the old `mode: ""` only so that
					// files written before this fix still load; new files must not contain it.
					if strings.Contains(string(out), `mode: ""`) {
						t.Errorf("wrote an empty mode for %+v:\n%s", in, out)
					}
					var back struct {
						Auth Auth `yaml:"auth"`
					}
					if err := yaml.Unmarshal(out, &back); err != nil {
						t.Errorf("wrote %+v as\n%s\nbut could not read it back: %v", in, out, err)
						continue
					}
					if !reflect.DeepEqual(normalize(in), normalize(back.Auth)) {
						t.Errorf("round trip changed %+v into %+v\n%s", in, back.Auth, out)
					}
				}
			}
		}
	}
}

func normalize(a Auth) Auth {
	if len(a.Groups) == 0 {
		a.Groups = nil
	}
	if len(a.BypassPaths) == 0 {
		a.BypassPaths = nil
	}
	return a
}

// A file an OLDER hemma wrote (`mode: ""`) still loads, as none; but a mapping with NO mode key at all is
// still refused, so `auth: {groups: [x]}` can never silently read as "no auth".
func TestAuth_LegacyEmptyModeLoads_MissingModeStillRefused(t *testing.T) {
	read := func(doc string) (Auth, error) {
		var v struct {
			Auth Auth `yaml:"auth"`
		}
		err := yaml.Unmarshal([]byte(doc), &v)
		return v.Auth, err
	}
	a, err := read("auth:\n  mode: \"\"\n  bypass_paths: [/api]\n")
	if err != nil || a.Mode != AuthNone || len(a.BypassPaths) != 1 {
		t.Errorf("legacy empty mode: %+v, %v", a, err)
	}
	if _, err := read("auth:\n  groups: [admins]\n"); err == nil {
		t.Error("a mapping without any mode key must still be refused")
	}
}

// End to end through the real file: the exact shape of the reported failure loads again.
func TestLoad_FileWithNoneModeAndBypassPathsIsReadable(t *testing.T) {
	dir := t.TempDir()
	doc := "hosts:\n  pi: {ip: 192.0.2.10}\ndomains:\n  - example.com\ndefaults:\n  dns_host: pi\nservices:\n  lol:\n    fqdn: lol.example.com\n    host: pi\n    backend: test:80\n    auth:\n      mode: \"\"\n      bypass_paths:\n        - /api\n      policy:\n        local: bypass\n        public: bypass\n"
	p := filepath.Join(dir, "services.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Services["lol"].Auth.BypassPaths; len(got) != 1 || got[0] != "/api" {
		t.Errorf("bypass paths lost: %v", got)
	}
	// and what hemma writes now is readable too
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("hemma wrote a file it cannot read back: %v", err)
	}
}
