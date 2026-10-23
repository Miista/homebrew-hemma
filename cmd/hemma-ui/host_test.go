package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hemma/internal/cli/kernel"
	"hemma/internal/config"
)

// setDeployHost rewrites the test repo's defaults.deploy_host (empty: unset).
func setDeployHost(t *testing.T, s *server, host string) {
	t.Helper()
	line := ""
	if host != "" {
		line = "  deploy_host: " + host + "\n"
	}
	cfg := "hosts:\n  resolver: {ip: 192.0.2.1}\n  appbox: {ip: 192.0.2.2}\ndomains:\n  - example.com\ndefaults:\n  dns_host: resolver\n" + line + "services: {}\n"
	if err := os.WriteFile(filepath.Join(s.repo, "services.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The UI's own host is the deploy host — never a guess from the network interfaces. Unset, or naming a host that does not exist,
// it is "": the kernel's "matches no declared host".
func TestDeployHostSelf(t *testing.T) {
	hosts := map[string]config.Host{"resolver": {IP: "192.0.2.1"}, "appbox": {IP: "192.0.2.2"}}
	for _, c := range []struct{ deploy, want string }{{"appbox", "appbox"}, {"resolver", "resolver"}, {"", ""}, {"ghost", ""}} {
		cfg := &config.Config{Hosts: hosts, Defaults: config.Defaults{DeployHost: c.deploy}}
		if got := deployHostSelf(cfg); got != c.want {
			t.Errorf("deploy_host %q: self = %q, want %q", c.deploy, got, c.want)
		}
	}
}

// Wired into the kernel, the state the page reads (what a deploy would do HERE) follows deploy_host: the resolver restarts
// pihole, another host does not, and with no deploy host there is no "here" at all.
func TestState_HostFollowsDeployHost(t *testing.T) {
	defer kernel.SetLocalHost(deployHostSelf)()
	s := repoWithConfig(t)
	here := func() map[string]any {
		rec := httptest.NewRecorder()
		s.state(rec, httptest.NewRequest("GET", "/api/state", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("state: %v\n%s", err, rec.Body.String())
		}
		return out["apply"].(map[string]any)
	}
	setDeployHost(t, s, "resolver")
	if a := here(); a["host"] != "resolver" || len(a["steps"].([]any)) != 1 {
		t.Errorf("deploy host = the resolver: %v", a)
	}
	setDeployHost(t, s, "appbox")
	if a := here(); a["host"] != "appbox" || strings.Contains(fmt.Sprint(a["steps"]), "pihole") {
		t.Errorf("deploy host = a service host: %v", a)
	}
	setDeployHost(t, s, "")
	if a := here(); a["host"] != "" || len(a["steps"].([]any)) != 0 {
		t.Errorf("no deploy host: %v", a)
	}
}
