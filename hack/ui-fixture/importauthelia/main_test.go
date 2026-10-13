package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic only: documentation domains and obviously fake secrets. Never put a real configuration.yml in a test.
const sourceServices = "hosts:\n  edge: {ip: 203.0.113.5}\ndomains:\n  - real.example.net\nservices:\n  app:\n    fqdn: app.real.example.net\n    host: edge\n    backend: app:80\n"

const sourceConfig = `server:
  address: tcp://:9091
session:
  secret: SESSION-SECRET-VALUE
  cookies:
    - domain: real.example.net
identity_validation:
  reset_password:
    jwt_secret: JWT-SECRET-VALUE
storage:
  encryption_key: STORAGE-KEY-VALUE
notifier:
  smtp:
    address: smtp://mail.real.example.net:25
    password: SMTP-PASSWORD-VALUE
identity_providers:
  oidc:
    hmac_secret: HMAC-VALUE
    jwks:
      - key: |
          -----BEGIN RSA PRIVATE KEY-----
          MIIE-FAKE-KEY
          -----END RSA PRIVATE KEY-----
    clients:
      - client_id: REAL-CLIENT-ID-1234
        client_name: App
        client_secret: '$pbkdf2-sha512$310000$fakefakefake'
        authorization_policy: app
        redirect_uris:
          - https://app.real.example.net/oauth/callback
          - https://elsewhere.unrelated.test/cb
      - client_id: another
        authorization_policy: one_factor
        redirect_uris:
          - https://elsewhere.unrelated.test/only
`

func run(t *testing.T, cfg string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "services.yaml")
	if err := os.WriteFile(sp, []byte(sourceServices), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := convert([]byte(cfg), sp, "example.com")
	return string(out), err
}

// Only the three fields hemma reads survive; every secret and every real name is gone.
func TestConvert_KeepsOnlyWhatHemmaReads(t *testing.T) {
	out, err := run(t, sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SESSION-SECRET", "JWT-SECRET", "STORAGE-KEY", "SMTP-PASSWORD", "HMAC-VALUE", "PRIVATE KEY", "MIIE-FAKE", "pbkdf2",
		"REAL-CLIENT-ID", "client_name", "real.example.net", "unrelated.test", "smtp", "cookies"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived:\n%s", leak, out)
		}
	}
	for _, want := range []string{"client_id: app", "authorization_policy: app", "https://app.example.com/oauth/callback"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// the client whose only redirect URI is outside the declared domains is dropped whole
	if strings.Contains(out, "another") || strings.Contains(out, "one_factor") {
		t.Errorf("a client with no redirect URI under a declared domain must be dropped:\n%s", out)
	}
}

// The safety net refuses when a secret-looking word ends up in a KEPT field (here, a policy name).
func TestConvert_RefusesWhenSomethingSecretLookingSurvives(t *testing.T) {
	bad := strings.Replace(sourceConfig, "authorization_policy: app", "authorization_policy: my-secret-policy", 1)
	if _, err := run(t, bad); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// A real domain inside a kept field (a path here) is caught too.
func TestConvert_RefusesWhenARealDomainSurvives(t *testing.T) {
	bad := strings.Replace(sourceConfig, "/oauth/callback", "/to/real.example.net", 1)
	if _, err := run(t, bad); err == nil || !strings.Contains(err.Error(), "real domain") {
		t.Fatalf("want a refusal, got %v", err)
	}
}
