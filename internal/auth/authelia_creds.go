package auth

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/go-crypt/crypt/algorithm/argon2"
	"github.com/go-crypt/crypt/algorithm/pbkdf2"
)

// Credential generation for the Authelia provider. Digests are produced with
// github.com/go-crypt/crypt — the exact library Authelia itself uses — so the
// crypt-format encodings (including pbkdf2's adapted-base64 alphabet) are
// byte-compatible with `authelia crypto hash validate` by construction.
//
// Parameters mirror `authelia crypto hash generate` defaults:
//   - OIDC client secrets: PBKDF2-SHA512, 310000 iterations, 16-byte salt,
//     64-byte key -> $pbkdf2-sha512$310000$<salt>$<key>
//   - user passwords: argon2id, m=65536 (64MB), t=3, p=4, 16-byte salt,
//     32-byte key -> $argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>

// randCharset is Authelia's `rfc3986` charset (RFC 3986 unreserved characters).
const randCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// randLen is the length Authelia's docs use for both OIDC client ids and
// client secrets.
const randLen = 72

// randString returns n crypto/rand characters from randCharset, using
// rejection sampling so every character is uniformly likely (no modulo bias).
func randString(n int) (string, error) {
	const max = byte(len(randCharset) * (256 / len(randCharset))) // largest unbiased multiple
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		for _, b := range buf {
			if b < max && len(out) < n {
				out = append(out, randCharset[int(b)%len(randCharset)])
			}
		}
	}
	return string(out), nil
}

// hashPBKDF2SHA512 digests password with Authelia's OIDC client-secret
// defaults (PBKDF2-SHA512, 310000 iterations, 16-byte salt, 64-byte key).
func hashPBKDF2SHA512(password string) (string, error) {
	// Variant first: WithKeyLength validates against the variant's HMAC size.
	hasher, err := pbkdf2.New(
		pbkdf2.WithVariant(pbkdf2.VariantSHA512),
		pbkdf2.WithIterations(310000),
		pbkdf2.WithSaltLength(16),
		pbkdf2.WithKeyLength(64),
	)
	if err != nil {
		return "", fmt.Errorf("init pbkdf2 hasher: %w", err)
	}
	d, err := hasher.Hash(password)
	if err != nil {
		return "", fmt.Errorf("hash secret: %w", err)
	}
	return d.Encode(), nil
}

// GenerateOIDCClient mints a 72-char client id and secret (rfc3986 charset)
// and the secret's PBKDF2-SHA512 crypt digest, matching what
// `authelia crypto hash generate pbkdf2 --variant sha512 --random` produces.
func (authelia) GenerateOIDCClient() (clientID, secret, digest string, err error) {
	if clientID, err = randString(randLen); err != nil {
		return "", "", "", err
	}
	if secret, err = randString(randLen); err != nil {
		return "", "", "", err
	}
	if digest, err = hashPBKDF2SHA512(secret); err != nil {
		return "", "", "", err
	}
	return clientID, secret, digest, nil
}

// OIDCClientSnippet renders the configuration.yml client entry for c,
// indented to paste directly under identity_providers.oidc.clients.
func (authelia) OIDCClientSnippet(c OIDCClient) string {
	var b strings.Builder
	b.WriteString("Add to identity_providers.oidc.clients in configuration.yml:\n\n")
	fmt.Fprintf(&b, "      - client_id: %s\n", yq(c.ClientID))
	fmt.Fprintf(&b, "        client_name: %s\n", yq(c.Name))
	fmt.Fprintf(&b, "        client_secret: %s\n", yq(c.SecretDigest))
	b.WriteString("        public: false\n")
	b.WriteString("        consent_mode: 'implicit'\n")
	fmt.Fprintf(&b, "        authorization_policy: %s\n", yq(c.Policy))
	b.WriteString("        redirect_uris:\n")
	fmt.Fprintf(&b, "          - %s\n", yq(c.RedirectURI))
	return b.String()
}

// HashUserPassword digests password with Authelia's file-backend defaults
// (argon2id, m=65536, t=3, p=4, 16-byte salt, 32-byte key).
func (authelia) HashUserPassword(password string) (string, error) {
	hasher, err := argon2.New(
		argon2.WithVariantID(),
		argon2.WithM(65536),
		argon2.WithT(3),
		argon2.WithP(4),
		argon2.WithS(16),
		argon2.WithK(32),
	)
	if err != nil {
		return "", fmt.Errorf("init argon2 hasher: %w", err)
	}
	d, err := hasher.Hash(password)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return d.Encode(), nil
}

// UserSnippet renders the users_database.yml entry for a new user. splitdns
// never writes that file — it is hand-owned and secret-bearing — so this is
// paste-in instructions only.
func (authelia) UserSnippet(username, email, digest string) string {
	var b strings.Builder
	b.WriteString("Add to users_database.yml:\n\n")
	fmt.Fprintf(&b, "  %s:\n", username)
	b.WriteString("    disabled: false\n")
	fmt.Fprintf(&b, "    displayname: %s\n", username)
	fmt.Fprintf(&b, "    email: %s\n", email)
	fmt.Fprintf(&b, "    password: %s\n", yq(digest))
	b.WriteString("    groups: []\n")
	return b.String()
}
