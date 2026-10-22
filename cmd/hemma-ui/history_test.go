package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A service's history is read from git: its own commits, newest first, followed back through a rename, and never another
// service's.
func TestHistory_FollowsOneServiceThroughARename(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	s := &server{repo: dir}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git not available: %v %s", err, out)
	}
	commit := func(msg, yaml string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
			if out, err := s.git(time.Minute, a...); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
	}
	base := "hosts:\n    pi:\n        ip: 192.0.2.10\ndomains:\n    - example.com\nservices:\n"
	svc := func(name, backend string) string {
		return "    " + name + ":\n        fqdn: " + name + ".example.com\n        host: pi\n        backend: " + backend + "\n"
	}
	other := "    other:\n        fqdn: other.example.com\n        host: pi\n        backend: other:1\n"
	commit("Add albumreq", base+svc("albumreq", "albumreq:80")+other)
	commit("Point albumreq at a new port", base+svc("albumreq", "albumreq:8080")+other)
	commit("Touch another service", base+svc("albumreq", "albumreq:8080")+strings.Replace(other, "other:1", "other:2", 1))
	commit("Rename albumreq to albumarr", base+svc("albumarr", "albumarr:8080")+strings.Replace(other, "other:1", "other:2", 1))
	commit("Move albumarr to 9090", base+svc("albumarr", "albumarr:9090")+strings.Replace(other, "other:1", "other:2", 1))

	res, err := s.history("albumarr", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Entries {
		got = append(got, e.Status+": "+e.Subject)
	}
	want := []string{"changed: Move albumarr to 9090", "renamed: Rename albumreq to albumarr", "changed: Point albumreq at a new port", "added: Add albumreq"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history of albumarr:\n got  %q\n want %q", got, want)
	}
	if e := res.Entries[1]; e.From != "albumreq" || e.Fields[0].Field != "Name" || e.Fields[0].Before != "albumreq" || e.Fields[0].After != "albumarr" {
		t.Errorf("the rename entry: %+v", e)
	}
	if e := res.Entries[0]; len(e.Fields) != 1 || e.Fields[0].Field != "Backend" || e.Fields[0].Before != "albumarr:8080" || e.Fields[0].After != "albumarr:9090" {
		t.Errorf("the backend change shows only what changed: %+v", e.Fields)
	}
	// the name the service had at each commit: the commit messages of an older entry mention the old name
	if got := []string{res.Entries[0].Name, res.Entries[1].Name, res.Entries[2].Name, res.Entries[3].Name}; strings.Join(got, ",") != "albumarr,albumarr,albumreq,albumreq" {
		t.Errorf("names per entry: %v", got)
	}
	if e := res.Entries[0]; e.Author == "" || e.When == "" || len(e.Short) != 7 {
		t.Errorf("who, when and which commit: %+v", e)
	}
	// the other service has its own, shorter history
	if r2, _ := s.history("other", 0, ""); len(r2.Entries) != 2 || r2.Entries[0].Subject != "Touch another service" {
		t.Errorf("history of other: %+v", r2.Entries)
	}
	// a name that never existed has none, and a bad name is refused by the handler
	if r3, _ := s.history("nope", 0, ""); len(r3.Entries) != 0 {
		t.Errorf("history of nope: %+v", r3.Entries)
	}
}

// The whole history is every Save with everything it changed, newest first; a commit that changed no value is left out.
func TestHistoryAll_EveryCommitWithItsChanges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	s := &server{repo: dir}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git not available: %v %s", err, out)
	}
	commit := func(msg, yaml string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
			if out, err := s.git(time.Minute, a...); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
	}
	hosts := "hosts:\n    pi:\n        ip: 192.0.2.10\ndomains:\n    - example.com\nservices:\n"
	svc := func(name, backend string) string {
		return "    " + name + ":\n        fqdn: " + name + ".example.com\n        host: pi\n        backend: " + backend + "\n"
	}
	commit("pi/a: add", hosts+svc("a", "a:1"))
	commit("pi/a: port", hosts+svc("a", "a:2"))
	commit("whitespace only", "# a comment\n"+hosts+svc("a", "a:2"))
	commit("pi/b: add one and move a", hosts+svc("a", "a:3")+svc("b", "b:1"))

	res, err := s.historyAll(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Entries {
		var ch []string
		for _, c := range e.Changes {
			ch = append(ch, c.Status+" "+c.Kind+" "+c.Name)
		}
		got = append(got, e.Subject+" => "+strings.Join(ch, ", "))
	}
	want := []string{
		"pi/b: add one and move a => changed Service a, added Service b",
		"pi/a: port => changed Service a",
		"pi/a: add => added Service a, added Host pi, added Domain example.com",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("history of everything:\n got  %q\n want %q", got, want)
	}
	if e := res.Entries[1]; e.Author == "" || e.When == "" || len(e.Short) != 7 {
		t.Errorf("who, when, which commit: %+v", e)
	}
}

// The whole history is read a page at a time, each page saying where the next one starts.
func TestHistoryAll_Pages(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	s := &server{repo: dir}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git not available: %v %s", err, out)
	}
	for i := 1; i <= 5; i++ {
		y := "hosts:\n    pi:\n        ip: 192.0.2.10\ndomains:\n    - example.com\nservices:\n    a:\n        fqdn: a.example.com\n        host: pi\n        backend: a:" + strconv.Itoa(i) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(y), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "save " + strconv.Itoa(i)}} {
			if out, err := s.git(time.Minute, a...); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
	}
	var seen []string
	skip := 0
	for page := 1; page <= 3; page++ {
		r, _ := s.historyAll(skip, 2)
		for _, e := range r.Entries {
			seen = append(seen, e.Subject)
		}
		wantMore := page < 3
		if r.More != wantMore || (r.Next != nil) != wantMore {
			t.Fatalf("page %d: more=%v next=%+v", page, r.More, r.Next)
		}
		if r.Next != nil {
			skip = r.Next.Skip
		}
	}
	if strings.Join(seen, ",") != "save 5,save 4,save 3,save 2,save 1" {
		t.Errorf("the pages together: %v", seen)
	}
}

// One service's changes are read in chunks, and carrying on after a rename keeps following the old name.
func TestHistory_PagesFollowARenameAcrossThem(t *testing.T) {
	oldChunk, oldWant := historyChunk, historyWant
	historyChunk, historyWant = 2, 1
	defer func() { historyChunk, historyWant = oldChunk, oldWant }()
	dir := filepath.Join(t.TempDir(), "repo")
	s := &server{repo: dir}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git not available: %v %s", err, out)
	}
	commit := func(msg, yaml string) {
		if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
			if out, err := s.git(time.Minute, a...); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
	}
	base := "hosts:\n    pi:\n        ip: 192.0.2.10\ndomains:\n    - example.com\nservices:\n"
	svc := func(n, b string) string {
		return "    " + n + ":\n        fqdn: " + n + ".example.com\n        host: pi\n        backend: " + b + "\n"
	}
	other := func(v string) string {
		return "    other:\n        fqdn: other.example.com\n        host: pi\n        backend: other:" + v + "\n"
	}
	commit("add", base+svc("old", "old:1")+other("1"))
	commit("port", base+svc("old", "old:2")+other("1"))
	commit("noise", base+svc("old", "old:2")+other("2"))
	commit("rename", base+svc("new", "new:2")+other("2"))
	commit("port again", base+svc("new", "new:3")+other("2"))

	var got []string
	skip, name := 0, ""
	for pages := 0; pages < 10; pages++ {
		r, _ := s.history("new", skip, name)
		for _, e := range r.Entries {
			got = append(got, e.Status+":"+e.Subject)
		}
		if !r.More {
			break
		}
		skip, name = r.Next.Skip, r.Next.Name
	}
	if strings.Join(got, ",") != "changed:port again,renamed:rename,changed:port,added:add" {
		t.Errorf("a service's history across pages: %v", got)
	}
}
