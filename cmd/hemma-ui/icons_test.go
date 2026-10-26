package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// Both icon links in the page carry a hash of their file, so a changed icon gets a new URL — the only thing a browser's
// favicon store re-fetches on.
func TestVersionIcons_EveryIconLinkCarriesItsHash(t *testing.T) {
	raw, err := static.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(versionIcons(raw))
	for _, name := range []string{"icon.svg", "favicon.ico"} {
		b, err := static.ReadFile("static/icons/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want := `href="/static/icons/` + name + "?v=" + hex.EncodeToString(sum[:6]) + `"`
		if !strings.Contains(page, want) {
			t.Errorf("page does not link %s", want)
		}
		if strings.Contains(page, `href="/static/icons/`+name+`"`) {
			t.Errorf("an unversioned link to %s is left", name)
		}
	}
}
