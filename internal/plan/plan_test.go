package plan

import (
	"strings"
	"testing"

	"splitdns/internal/config"
)

// base returns a valid config with one service we can mutate per test.
func base() *config.Config {
	return &config.Config{
		Hosts: map[string]config.Host{
			"resolver": {IP: "192.0.2.1", Dir: "resolver"},
			"appbox":   {IP: "192.0.2.2", Dir: "appbox"},
		},
		Domains: map[string]config.Domain{
			"example.com": {},
			"example.net": {},
		},
		Defaults: config.Defaults{DNSHost: "resolver"},
		Services: map[string]config.Service{},
	}
}

func TestBuild_ValidService(t *testing.T) {
	c := base()
	c.Services["docs"] = config.Service{FQDN: "docs.example.com", Host: "appbox", Backend: "paperless:8000"}

	p := Build(c)
	if len(p.Skipped) != 0 {
		t.Fatalf("unexpected skips: %v", p.Skipped)
	}
	files := p.Files["docs"]
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}

	byExt := map[string]File{}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, ".conf"):
			byExt["conf"] = f
		case strings.HasSuffix(f.Path, ".caddy"):
			byExt["caddy"] = f
		}
	}

	// DNS goes to the dns_host (resolver) dir; A record points at the host (appbox) IP.
	conf := byExt["conf"]
	if conf.Path != "resolver/"+config.DefaultDnsmasqDir+"/docs.generated.conf" {
		t.Errorf("dns path wrong: %q", conf.Path)
	}
	if !strings.Contains(conf.Content, "192.0.2.2") {
		t.Errorf("A record should point at host (appbox) IP: %q", conf.Content)
	}

	// Caddy goes to the host (appbox) dir with the matched tls import.
	caddy := byExt["caddy"]
	if caddy.Path != "appbox/"+config.DefaultCaddySitesDir+"/docs.caddy" {
		t.Errorf("caddy path wrong: %q", caddy.Path)
	}
	if !strings.Contains(caddy.Content, "import tls_example_com") {
		t.Errorf("wrong tls import: %q", caddy.Content)
	}
}

func TestBuild_SkipReasons(t *testing.T) {
	tests := []struct {
		name    string
		svc     config.Service
		wantSub string
	}{
		{"malformed-fqdn", config.Service{FQDN: "", Host: "resolver", Backend: "a:1"}, "malformed fqdn"},
		{"no-domain-match", config.Service{FQDN: "x.example.org", Host: "resolver", Backend: "a:1"}, "matches no domain"},
		{"unknown-host", config.Service{FQDN: "x.example.com", Host: "nope", Backend: "a:1"}, "unknown host"},
		{"bad-backend", config.Service{FQDN: "x.example.com", Host: "resolver", Backend: "noport"}, "name:port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			c.Services["svc"] = tt.svc
			p := Build(c)
			reason, ok := p.Skipped["svc"]
			if !ok {
				t.Fatalf("expected skip, got files: %+v", p.Files["svc"])
			}
			if !strings.Contains(reason, tt.wantSub) {
				t.Errorf("reason %q does not contain %q", reason, tt.wantSub)
			}
		})
	}
}

func TestBuild_FQDNCollision_FailsBoth(t *testing.T) {
	c := base()
	c.Services["a"] = config.Service{FQDN: "dup.example.com", Host: "resolver", Backend: "x:1"}
	c.Services["b"] = config.Service{FQDN: "dup.example.com", Host: "appbox", Backend: "y:2"}

	p := Build(c)
	if _, ok := p.Skipped["a"]; !ok {
		t.Error("service a should be skipped on fqdn collision")
	}
	if _, ok := p.Skipped["b"]; !ok {
		t.Error("service b should be skipped on fqdn collision")
	}
	for k := range p.Files {
		if !IsSyntheticOwner(k) {
			t.Errorf("no service files should be produced on collision, got %q: %+v", k, p.Files[k])
		}
	}
}

func TestBuild_LongestDomainSuffixWins(t *testing.T) {
	c := base()
	c.Domains["sub.example.com"] = config.Domain{}
	c.Services["s"] = config.Service{FQDN: "a.sub.example.com", Host: "resolver", Backend: "x:1"}

	p := Build(c)
	files := p.Files["s"]
	if len(files) == 0 {
		t.Fatalf("expected files, skipped: %v", p.Skipped)
	}
	var caddy string
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".caddy") {
			caddy = f.Content
		}
	}
	if !strings.Contains(caddy, "import tls_sub") {
		t.Errorf("longest suffix (sub.example.com) should win: %q", caddy)
	}
}
