package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hemma/internal/config"
)

// The history is read straight from git: every Save is a commit of services.yaml with the person's message, so walking the
// commits that touched it, and comparing each version with the one before, says who changed what and when. Nothing is stored by
// hemma. (Unsaved changes are not in git yet, so they are not listed.)
//
// It is read a page at a time — each commit is a `git show` and a parse — and the page says where the next one starts.

var (
	historyPage  = 30  // saves in a page of the whole history
	historyChunk = 40  // commits read at a time while looking for one service's changes
	historyWant  = 20  // changes to one service to find before a page is returned
	historyScan  = 200 // commits one request may read while looking for them
)

type historyEntry struct {
	Commit  string        `json:"commit"`
	Short   string        `json:"short"`
	Name    string        `json:"name"` // the service's name as of this commit (before a rename it is the old name)
	Author  string        `json:"author"`
	When    string        `json:"when"` // ISO 8601
	Subject string        `json:"subject"`
	Body    string        `json:"body,omitempty"`
	Status  string        `json:"status"`         // added | changed | renamed | removed
	From    string        `json:"from,omitempty"` // renamed: the name before
	Fields  []fieldChange `json:"fields"`
}

// historyNext says where to carry on: how many commits are already read and, for one service, the name it had at that point
// (older entries are about its old name once a rename was passed).
type historyNext struct {
	Skip int    `json:"skip"`
	Name string `json:"name,omitempty"`
}

type historyResult struct {
	Service string         `json:"service"`
	Entries []historyEntry `json:"entries"`
	More    bool           `json:"more,omitempty"` // older commits are still to be read
	Next    *historyNext   `json:"next,omitempty"`
}

var historyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	skip, _ := strconv.Atoi(q.Get("skip"))
	if skip < 0 {
		skip = 0
	}
	name := q.Get("service")
	if name == "" { // no service: every Save, with everything it changed
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit < 1 || limit > 100 {
			limit = historyPage
		}
		res, err := s.historyAll(skip, limit)
		if err != nil {
			writeJSON(w, 200, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
		return
	}
	cur := q.Get("name") // the name this service had at the point the last page stopped
	if !historyName.MatchString(name) || (cur != "" && !historyName.MatchString(cur)) {
		writeJSON(w, 400, map[string]any{"error": "not a service name"})
		return
	}
	res, err := s.history(name, skip, cur)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

type gitCommit struct{ hash, author, when, subject, body string }

// historyVersions lists up to count commits that touched services.yaml, newest first, starting after the first skip of them, and
// services.yaml as of each. The version before commit i is the one at commit i+1 (the next older commit that touched the file), so
// one commit more is read just to give the last one its "before"; before the oldest of all there is nothing. more says older
// commits exist beyond these. Not a git repo, or no commits: nothing.
func (s *server) historyVersions(skip, count int) (commits []gitCommit, cfgAt []*config.Config, more bool) {
	if out, err := s.git(10*time.Second, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil, nil, false
	}
	// Five NUL-separated fields per commit, and -z ends each commit with a NUL too. NUL is the one byte git cannot
	// put in a message, author or date, so a message holding any other control character cannot shift the fields.
	const fields = 5
	out, err := s.git(30*time.Second, "log", "-z", "--skip="+strconv.Itoa(skip), "--max-count="+strconv.Itoa(count+1), "--format=%H%x00%an%x00%aI%x00%s%x00%b", "--", "services.yaml")
	if err != nil {
		return nil, nil, false
	}
	f := strings.Split(out, "\x00")
	for i := 0; i+fields <= len(f); i += fields {
		if f[i] == "" {
			break // the trailing terminator
		}
		commits = append(commits, gitCommit{f[i], f[i+1], f[i+2], f[i+3], strings.TrimSpace(f[i+4])})
	}
	empty := &config.Config{Services: map[string]config.Service{}}
	versions := make([]*config.Config, len(commits))
	for i, c := range commits {
		data, err := s.git(20*time.Second, "show", c.hash+":services.yaml")
		cfg, perr := config.Parse([]byte(data), "services.yaml")
		if err != nil || perr != nil {
			cfg = empty // a version that does not parse says nothing
		}
		versions[i] = cfg
	}
	if len(commits) > count { // the extra one is only the "before" of the last
		more = true
		return commits[:count], versions, true
	}
	return commits, append(versions, empty), false
}

type historyCommit struct {
	Commit  string         `json:"commit"`
	Short   string         `json:"short"`
	Author  string         `json:"author"`
	When    string         `json:"when"`
	Subject string         `json:"subject"`
	Body    string         `json:"body,omitempty"`
	Changes []entityChange `json:"changes"`
}

type historyAllResult struct {
	Entries []historyCommit `json:"entries"`
	More    bool            `json:"more,omitempty"`
	Next    *historyNext    `json:"next,omitempty"`
}

// historyAll is a page of the saves, newest first, with everything each one changed (services, hosts, domains, settings), by
// comparing each version of services.yaml with the one before it. A commit that changed no value (a formatting-only rewrite) is
// left out.
func (s *server) historyAll(skip, limit int) (historyAllResult, error) {
	res := historyAllResult{Entries: []historyCommit{}}
	commits, cfgAt, more := s.historyVersions(skip, limit)
	for i, c := range commits {
		changes := diffEntities(cfgAt[i+1], cfgAt[i])
		if len(changes) == 0 {
			continue
		}
		res.Entries = append(res.Entries, historyCommit{Commit: c.hash, Short: c.hash[:min(7, len(c.hash))], Author: c.author, When: c.when, Subject: c.subject, Body: c.body, Changes: changes})
	}
	res.More = more
	if more {
		res.Next = &historyNext{Skip: skip + len(commits)}
	}
	return res, nil
}

// history lists the commits that changed the service, newest first, a page at a time. It reads commits in chunks until it has
// found enough of the service's changes (a service may go a long time untouched) or has read historyScan of them; Next says where to
// carry on. A rename is followed: before the commit that renamed it, the entries are about the old name (cur, when carrying on).
func (s *server) history(service string, skip int, cur string) (historyResult, error) {
	res := historyResult{Service: service, Entries: []historyEntry{}}
	if cur == "" {
		cur = service
	}
	next, scanned := skip, 0
	for len(res.Entries) < historyWant && scanned < historyScan {
		commits, cfgAt, more := s.historyVersions(next, historyChunk)
		for i, c := range commits {
			now, before := cfgAt[i].Services, cfgAt[i+1].Services
			e := historyEntry{Name: cur, Commit: c.hash, Short: c.hash[:min(7, len(c.hash))], Author: c.author, When: c.when, Subject: c.subject, Body: c.body}
			nv, inNow := now[cur]
			ov, inBefore := before[cur]
			switch {
			case inNow && inBefore:
				f := compare(serviceKV(ov), serviceKV(nv), "changed")
				if len(f) == 0 {
					continue
				}
				e.Status, e.Fields = "changed", f
			case inNow:
				if from, ok := findRenames(before, now)[cur]; ok {
					e.Status, e.From = "renamed", from
					e.Fields = append([]fieldChange{{Field: "Name", Before: from, After: cur}}, compare(serviceKV(before[from]), serviceKV(nv), "changed")...)
					cur = from // older entries are about the old name
				} else {
					e.Status, e.Fields = "added", compare(nil, serviceKV(nv), "added")
				}
			case inBefore:
				e.Status, e.Fields = "removed", compare(serviceKV(ov), serviceKV(ov), "removed")
			default:
				continue
			}
			res.Entries = append(res.Entries, e)
		}
		next += len(commits)
		scanned += len(commits)
		res.More = more
		if !more {
			break
		}
	}
	if res.More {
		res.Next = &historyNext{Skip: next, Name: cur}
	}
	return res, nil
}
