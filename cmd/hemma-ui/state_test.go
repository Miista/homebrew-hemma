package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// auth-bypass is where the EFFECTIVE policy skips sign-in, resolved by config's own rule rather than the page: a bypass at
// home is "local", and a bypass that also reaches the public horizon is "everywhere" — no sign-in at all, which the list draws
// like mode none. Without the tunnel an unset public follows local, so a local-only `local: bypass` is "everywhere" too.
func TestState_AuthBypassIsTheEffectivePolicy(t *testing.T) {
	s := repoWithConfig(t)
	cfg := `hosts:
  appbox: {ip: 192.0.2.2}
domains:
  - example.com
services:
  plain:     {fqdn: plain.example.com, host: appbox, backend: plain:80, auth: forward}
  home:      {fqdn: home.example.com, host: appbox, backend: home:80, tunnel: true, auth: {mode: forward, policy: {local: bypass, public: one_factor}}}
  homeonly:  {fqdn: homeonly.example.com, host: appbox, backend: homeonly:80, auth: {mode: forward, policy: {local: bypass}}}
  both:      {fqdn: both.example.com, host: appbox, backend: both:80, tunnel: true, auth: {mode: forward, policy: {local: bypass, public: bypass}}}
  open:      {fqdn: open.example.com, host: appbox, backend: open:80}
`
	if err := os.WriteFile(filepath.Join(s.repo, "services.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.state(rec, httptest.NewRequest("GET", "/api/state", nil))
	var out struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("state: %v\n%s", err, rec.Body.String())
	}
	got := map[string]any{}
	for _, sv := range out.Services {
		got[sv["name"].(string)] = sv["auth-bypass"]
	}
	for name, want := range map[string]string{"plain": "", "home": "local", "homeonly": "everywhere", "both": "everywhere", "open": ""} {
		if got[name] != want {
			t.Errorf("%s: auth-bypass = %v, want %q", name, got[name], want)
		}
	}
}

// /api/signin gives a service form's current values the same answer /api/state gives a saved service, so the form's sign-in
// icon never disagrees with the row it will become.
func TestSignin_SameAnswerAsState(t *testing.T) {
	s := repoWithConfig(t)
	for query, want := range map[string]string{
		"mode=forward": "",
		"mode=forward&tunnel=true&local=bypass&public=one_factor": "local",
		"mode=forward&local=bypass":                               "everywhere",
		"mode=forward&tunnel=true&local=bypass":                   "local",
		"mode=oidc&tunnel=true&local=bypass&public=bypass":        "everywhere",
		"mode=none&local=bypass&public=bypass":                    "",
		"mode=external&local=bypass":                              "",
	} {
		rec := httptest.NewRecorder()
		s.signin(rec, httptest.NewRequest("GET", "/api/signin?"+query, nil))
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
		if out["auth-bypass"] != want {
			t.Errorf("%s: auth-bypass = %q, want %q", query, out["auth-bypass"], want)
		}
	}
}
