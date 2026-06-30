package render

import (
	"strings"
	"testing"
)

func TestDNSRecord(t *testing.T) {
	got := DNSRecord("docs.example.com", "192.0.2.2")
	want := Header + "\n" +
		"local=/docs.example.com/\n" +
		"address=/docs.example.com/192.0.2.2\n" +
		"address=/docs.example.com/::\n"
	if got != want {
		t.Fatalf("DNSRecord mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// The :: vs ::1 distinction is structural (design §4.1): :: suppresses the
// public AAAA; ::1 is an explicit bug.
func TestDNSRecord_SuppressesAAAAWithUnspecified(t *testing.T) {
	got := DNSRecord("x.example.net", "192.0.2.1")
	if !strings.Contains(got, "address=/x.example.net/::\n") {
		t.Errorf("missing AAAA-suppression line: %q", got)
	}
	if strings.Contains(got, "::1") {
		t.Errorf("emitted ::1 (loopback) — must be :: (unspecified): %q", got)
	}
}

func TestCaddySite(t *testing.T) {
	got := CaddySite("docs.example.com", "tls_example_com", "paperless:8000")
	want := Header + "\n" +
		"docs.example.com {\n" +
		"\timport tls_example_com\n" +
		"\treverse_proxy paperless:8000\n" +
		"}\n"
	if got != want {
		t.Fatalf("CaddySite mismatch:\n got: %q\nwant: %q", got, want)
	}
}
