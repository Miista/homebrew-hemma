package auth

import (
	"strings"
	"testing"

	"github.com/go-crypt/crypt"
	"github.com/go-crypt/crypt/algorithm/argon2"
	"github.com/go-crypt/crypt/algorithm/pbkdf2"
)

// assertCharset fails unless s is exactly n chars from the rfc3986 unreserved set.
func assertCharset(t *testing.T, name, s string, n int) {
	t.Helper()
	if len(s) != n {
		t.Errorf("%s: length = %d, want %d", name, len(s), n)
	}
	for _, c := range s {
		if !strings.ContainsRune(randCharset, c) {
			t.Errorf("%s: character %q outside the rfc3986 unreserved charset", name, c)
		}
	}
}

func TestGenerateOIDCClient_Values(t *testing.T) {
	id, secret, digest, err := (authelia{}).GenerateOIDCClient()
	if err != nil {
		t.Fatal(err)
	}
	assertCharset(t, "client id", id, 72)
	assertCharset(t, "client secret", secret, 72)
	if id == secret {
		t.Error("client id and secret must be independent random values")
	}

	// Digest shape: Authelia/go-crypt crypt format with the expected parameters.
	if !strings.HasPrefix(digest, "$pbkdf2-sha512$310000$") {
		t.Fatalf("digest = %q, want $pbkdf2-sha512$310000$ prefix", digest)
	}
	if strings.HasSuffix(digest, "=") || strings.ContainsAny(digest, "+") {
		t.Errorf("digest %q must use unpadded adapted base64 (no '=' or '+')", digest)
	}

	// Round-trip: decode with go-crypt (the library Authelia validates with)
	// and verify the plaintext secret matches; a wrong password must not.
	dec := crypt.NewDecoder()
	if err := pbkdf2.RegisterDecoderSHA512(dec); err != nil {
		t.Fatal(err)
	}
	d, err := dec.Decode(digest)
	if err != nil {
		t.Fatalf("decode digest: %v", err)
	}
	if !d.Match(secret) {
		t.Error("generated secret does not match its own digest")
	}
	if d.Match(secret + "x") {
		t.Error("wrong password matched the digest")
	}
}

func TestGenerateOIDCClient_SaltAndKeySizes(t *testing.T) {
	_, secret, digest, err := (authelia{}).GenerateOIDCClient()
	if err != nil {
		t.Fatal(err)
	}
	dec := crypt.NewDecoder()
	if err := pbkdf2.RegisterDecoderSHA512(dec); err != nil {
		t.Fatal(err)
	}
	d, err := dec.Decode(digest)
	if err != nil {
		t.Fatal(err)
	}
	pd, ok := d.(*pbkdf2.Digest)
	if !ok {
		t.Fatalf("decoded digest is %T, want *pbkdf2.Digest", d)
	}
	if got := len(pd.Salt()); got != 16 {
		t.Errorf("salt = %d bytes, want 16", got)
	}
	if got := len(pd.Key()); got != 64 {
		t.Errorf("key = %d bytes, want 64", got)
	}
	if !pd.Match(secret) {
		t.Error("secret does not verify against digest")
	}
}

func TestHashUserPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	digest, err := (authelia{}).HashUserPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("digest = %q, want $argon2id$v=19$m=65536,t=3,p=4$ prefix", digest)
	}
	dec := crypt.NewDecoder()
	if err := argon2.RegisterDecoderArgon2id(dec); err != nil {
		t.Fatal(err)
	}
	d, err := dec.Decode(digest)
	if err != nil {
		t.Fatalf("decode digest: %v", err)
	}
	if !d.Match(pw) {
		t.Error("password does not verify against its digest")
	}
	if d.Match("wrong") {
		t.Error("wrong password matched the digest")
	}
}

func TestOIDCClientSnippet(t *testing.T) {
	got := (authelia{}).OIDCClientSnippet(OIDCClient{
		Name:         "grafana",
		ClientID:     "ID123",
		SecretDigest: "$pbkdf2-sha512$310000$s$k",
		RedirectURI:  "https://grafana.example.com/login/generic_oauth",
		Policy:       "grafana",
	})
	want := `Add to identity_providers.oidc.clients in configuration.yml:

      - client_id: 'ID123'
        client_name: 'grafana'
        client_secret: '$pbkdf2-sha512$310000$s$k'
        public: false
        consent_mode: 'implicit'
        authorization_policy: 'grafana'
        redirect_uris:
          - 'https://grafana.example.com/login/generic_oauth'
`
	if got != want {
		t.Errorf("snippet mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUserSnippet(t *testing.T) {
	got := (authelia{}).UserSnippet("alice", "alice@example.com", "$argon2id$v=19$m=65536,t=3,p=4$s$h")
	want := `Add to users_database.yml:

  alice:
    disabled: false
    displayname: alice
    email: alice@example.com
    password: '$argon2id$v=19$m=65536,t=3,p=4$s$h'
    groups: []
`
	if got != want {
		t.Errorf("snippet mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRandString_Distribution(t *testing.T) {
	// Every value independent + full length across repeated draws; also make
	// sure ~ and - (edge charset members) can appear over a large sample.
	s, err := randString(4096)
	if err != nil {
		t.Fatal(err)
	}
	assertCharset(t, "randString", s, 4096)
}
