package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hemma/internal/config"
)

// A service's history is read straight from git: every Save is a commit of services.yaml with the person's message, so
// walking the commits that touched it, and comparing the service between one version and the next, says who changed
// what and when. Nothing is stored by hemma. (Unsaved changes are not in git yet, so they are not listed.)

// historyScan caps how many commits of services.yaml one request reads: each is a `git show` and a parse.
const historyScan = 400

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

type historyResult struct {
	Service   string         `json:"service"`
	Entries   []historyEntry `json:"entries"`
	Truncated bool           `json:"truncated,omitempty"` // older commits exist beyond the scan
}

var historyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("service")
	if name == "" { // no service: every Save, with everything it changed
		res, err := s.historyAll()
		if err != nil {
			writeJSON(w, 200, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
		return
	}
	if !historyName.MatchString(name) {
		writeJSON(w, 400, map[string]any{"error": "not a service name"})
		return
	}
	res, err := s.history(name)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

type gitCommit struct{ hash, author, when, subject, body string }

// historyVersions lists the commits that touched services.yaml, newest first, and services.yaml as of each: the version before
// commit i is the one at commit i+1 (the next older commit that touched the file), and before the oldest there is nothing.
// Not a git repo, or no commits yet: nothing. truncated says older commits exist beyond the scan.
func (s *server) historyVersions() (commits []gitCommit, cfgAt []*config.Config, truncated bool) {
	if out, err := s.git(10*time.Second, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil, nil, false
	}
	const rec, sep = "\x1e", "\x1f"
	out, err := s.git(30*time.Second, "log", "--max-count="+strconv.Itoa(historyScan+1), "--format=%H"+sep+"%an"+sep+"%aI"+sep+"%s"+sep+"%b"+rec, "--", "services.yaml")
	if err != nil {
		return nil, nil, false
	}
	for _, r := range strings.Split(out, rec) {
		r = strings.TrimLeft(r, "\n")
		if r == "" {
			continue
		}
		f := strings.SplitN(r, sep, 5)
		if len(f) < 5 {
			continue
		}
		commits = append(commits, gitCommit{f[0], f[1], f[2], f[3], strings.TrimSpace(f[4])})
	}
	if len(commits) > historyScan {
		commits, truncated = commits[:historyScan], true
	}
	empty := &config.Config{Services: map[string]config.Service{}}
	cfgAt = make([]*config.Config, len(commits)+1)
	cfgAt[len(commits)] = empty
	for i, c := range commits {
		data, err := s.git(20*time.Second, "show", c.hash+":services.yaml")
		cfg, perr := config.Parse([]byte(data), "services.yaml")
		if err != nil || perr != nil {
			cfg = empty // a version that does not parse says nothing
		}
		cfgAt[i] = cfg
	}
	return commits, cfgAt, truncated
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
	Entries   []historyCommit `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// historyAll is every Save, newest first, with everything it changed (services, hosts, domains, settings), by comparing each
// version of services.yaml with the one before it. A commit that changed no value (a formatting-only rewrite) is left out.
func (s *server) historyAll() (historyAllResult, error) {
	res := historyAllResult{Entries: []historyCommit{}}
	commits, cfgAt, truncated := s.historyVersions()
	res.Truncated = truncated
	for i, c := range commits {
		changes := diffEntities(cfgAt[i+1], cfgAt[i])
		if len(changes) == 0 {
			continue
		}
		res.Entries = append(res.Entries, historyCommit{Commit: c.hash, Short: c.hash[:min(7, len(c.hash))], Author: c.author, When: c.when, Subject: c.subject, Body: c.body, Changes: changes})
	}
	return res, nil
}

// history lists the commits that changed the service, newest first. A rename is followed: before the commit that renamed
// it, the entries are about the old name.
func (s *server) history(service string) (historyResult, error) {
	res := historyResult{Service: service, Entries: []historyEntry{}}
	commits, cfgAt, truncated := s.historyVersions()
	res.Truncated = truncated
	cur := service
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
	return res, nil
}
