package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gatedServer is the real route table behind a real authy gate (local account,
// state in a temp dir), over a synthetic repo.
func gatedServer(t *testing.T) (http.Handler, *server) {
	t.Helper()
	t.Setenv("HEMMA_UI_DATA", t.TempDir())
	s := repoWithConfig(t)
	gate, err := newGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.routes(gate)
	if err != nil {
		t.Fatal(err)
	}
	return h, s
}

// Without a session, NOTHING is reachable: not the page, not the static files,
// not a single API route — reads or writes.
func TestGate_EveryRouteRefusedWithoutASession(t *testing.T) {
	h, _ := gatedServer(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/", ""},
		{"GET", "/static/style.css", ""},
		{"GET", "/static/fonts/geist-sans-400.woff2", ""},
		{"GET", "/api/state", ""},
		{"GET", "/api/commands", ""},
		{"GET", "/api/browse?path=.", ""},
		{"GET", "/api/changes", ""},
		{"GET", "/api/diff", ""},
		{"POST", "/api/discard", "{}"},
		{"POST", "/api/commit", `{"message":"x"}`},
		{"POST", "/api/push", "{}"},
		{"POST", "/api/run", `{"command":"list","values":{}}`},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		switch rec.Code {
		case http.StatusUnauthorized, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		default:
			t.Errorf("%s %s: want 401 or a redirect to sign-in, got %d", r.method, r.path, rec.Code)
		}
		// Whatever the gate answers, it must not be the app's data.
		for _, leak := range []string{"example.com", "== Hosts", "\"services\"", "static/style.css\">"} {
			if strings.Contains(body, leak) && !strings.Contains(strings.ToLower(body), "sign") {
				t.Errorf("%s %s leaked app content (%q): %s", r.method, r.path, leak, body)
			}
		}
	}
}

// A page load goes to sign-in (with first-launch setup when nobody has an
// account yet); an API call gets a plain 401 JSON the page can react to.
func TestGate_PageRedirectsAndAPIAnswers401(t *testing.T) {
	h, _ := gatedServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code/100 != 3 || !strings.Contains(loc, "/auth/") {
		t.Errorf("page load: want a redirect into /auth/, got %d Location=%q", rec.Code, loc)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Accept", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("API call: want 401, got %d", rec.Code)
	}
}

// A forged session cookie is not a session.
func TestGate_ForgedCookieIsRefused(t *testing.T) {
	h, _ := gatedServer(t)
	for _, c := range []string{"authy_session=admin", "session=eyJhbGciOiJub25lIn0.e30.", "authy=1"} {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Header.Set("Cookie", c)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("cookie %q was accepted", c)
		}
	}
	// HTTP basic auth, the old mechanism, must not work any more either.
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.SetBasicAuth("admin", "fixture")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("basic auth was accepted")
	}
}

// The UI refuses to start ungated: disabling authy is an error, not a mode.
func TestNewGate_RefusesToRunUngated(t *testing.T) {
	t.Setenv("HEMMA_UI_DATA", t.TempDir())
	t.Setenv("HEMMA_UI_AUTH_DISABLED", "true")
	if _, err := newGate(context.Background()); err == nil {
		t.Fatal("HEMMA_UI_AUTH_DISABLED must make startup fail")
	}
}

// Half-configured OIDC is an error too (authy requires all four together).
func TestNewGate_PartialOIDCIsAnError(t *testing.T) {
	t.Setenv("HEMMA_UI_DATA", t.TempDir())
	t.Setenv("HEMMA_UI_OIDC_ISSUER", "https://auth.example.com")
	if _, err := newGate(context.Background()); err == nil {
		t.Fatal("partial OIDC settings must make startup fail")
	}
}
