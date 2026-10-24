package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// servicesYAML builds a services.yaml with the given services (name → "fqdn host backend [extra auth lines]").
func servicesYAML(svcs ...[4]string) string {
	var b strings.Builder
	b.WriteString("hosts:\n    core:\n        ip: 192.0.2.2\n    edge:\n        ip: 192.0.2.50\ndomains:\n    - example.com\ndefaults:\n    dns_host: edge\nservices:\n")
	for _, s := range svcs { // name, fqdn, host, backend
		b.WriteString("    " + s[0] + ":\n        fqdn: " + s[1] + "\n        host: " + s[2] + "\n        backend: " + s[3] + "\n")
	}
	return b.String()
}

// diffAfter commits `before` and returns the diff of `after` against it.
func diffAfter(t *testing.T, before, after string) diffResult {
	t.Helper()
	dir := t.TempDir()
	s := &server{repo: dir}
	if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if out, err := s.git(time.Minute, a...); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := s.diff()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func serviceEntities(d diffResult) (names, statuses []string) {
	for _, e := range d.Entities {
		if e.Kind == "Service" {
			names, statuses = append(names, e.Name), append(statuses, e.Status)
		}
	}
	return
}

// A service that only changed its name is ONE change, not "one removed and one added" — whether its address and
// backend followed the name or stayed as they were.
func TestDiff_RenamedService(t *testing.T) {
	cases := map[string][2]string{
		"address and backend followed the name": {
			servicesYAML([4]string{"oldapp", "oldapp.example.com", "core", "oldapp:8768"}),
			servicesYAML([4]string{"newapp", "newapp.example.com", "core", "newapp:8768"}),
		},
		"only the name changed": {
			servicesYAML([4]string{"oldapp", "oldapp.example.com", "core", "oldapp:8768"}),
			servicesYAML([4]string{"newapp", "oldapp.example.com", "core", "oldapp:8768"}),
		},
		"address followed, backend stayed": {
			servicesYAML([4]string{"oldapp", "oldapp.example.com", "core", "oldapp:8768"}),
			servicesYAML([4]string{"newapp", "newapp.example.com", "core", "oldapp:8768"}),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			names, statuses := serviceEntities(diffAfter(t, c[0], c[1]))
			if len(names) != 1 || statuses[0] != "renamed" || names[0] != "oldapp → newapp" {
				t.Errorf("want one 'renamed' service 'oldapp → newapp', got %v %v", names, statuses)
			}
		})
	}

	// the renamed card lists the name first, then only what else changed
	d := diffAfter(t, cases["address and backend followed the name"][0], cases["address and backend followed the name"][1])
	var fields []string
	for _, f := range d.Entities[0].Fields {
		fields = append(fields, f.Field+": "+f.Before+" → "+f.After)
	}
	want := []string{"Name: oldapp → newapp", "Address: oldapp.example.com → newapp.example.com", "Backend: oldapp:8768 → newapp:8768"}
	if strings.Join(fields, "|") != strings.Join(want, "|") {
		t.Errorf("fields = %v, want %v", fields, want)
	}
}

// What is NOT a rename must stay removed + added: a service replaced by an unrelated one, even on the same host, and
// two services where only the host matches. A wrong pairing would hide a real removal and a real addition.
func TestDiff_NotARename(t *testing.T) {
	for name, c := range map[string][2]string{
		"replaced by an unrelated service": {
			servicesYAML([4]string{"web", "web.example.com", "edge", "web:80"}),
			servicesYAML([4]string{"docsapp", "docs.example.com", "edge", "docsapp:8000"}),
		},
		"same name pattern but another host": {
			servicesYAML([4]string{"oldapp", "oldapp.example.com", "core", "oldapp:8768"}),
			servicesYAML([4]string{"newapp", "newapp.example.com", "edge", "newapp:8768"}),
		},
		"address unrelated to the name": {
			servicesYAML([4]string{"oldapp", "oldapp.example.com", "core", "oldapp:8768"}),
			servicesYAML([4]string{"newapp", "music.example.com", "core", "newapp:8768"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			names, statuses := serviceEntities(diffAfter(t, c[0], c[1]))
			if len(names) != 2 {
				t.Fatalf("want a removed and an added service, got %v %v", names, statuses)
			}
			got := statuses[0] + "," + statuses[1]
			if got != "added,removed" && got != "removed,added" {
				t.Errorf("statuses = %s", got)
			}
		})
	}
}
