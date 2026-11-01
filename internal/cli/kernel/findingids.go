package kernel

import (
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"hemma/internal/config"
)

// A finding's ID names what it is about, never who reported it: <type>:<scope>:<subject>.
//
//	type     the kind of finding, dotted and lower case ("drift", "caddy.import", "auth.bypass.both")
//	scope    the host (folder) the subject belongs to, or "repo" when it belongs to none
//	subject  the service, file or setting it is about ("jellyfin", "optiplex/caddy/data/sites/jellyfin.caddy", "Caddyfile")
//
// So the same problem found from two machines has the same ID — which is what lets DiagnoseFleet show it once — and an ID never
// depends on the order findings come out in or on how many others there are (no "advisory 3"). Design §6.4.

const repoScope = "repo"

var findingIDShape = regexp.MustCompile(`^[a-z][a-z0-9._-]*:[A-Za-z0-9._-]+:[A-Za-z0-9._/@-]+$`)

func findingID(typ, scope, subject string) string { return typ + ":" + scope + ":" + subject }

// currentID reports whether id is in the scheme above. An ID that is not comes from an older hemma (or is missing) and is never
// merged with another host's: nothing says the two are the same problem.
func currentID(id string) bool { return findingIDShape.MatchString(id) }

// scopeOfPath is the host whose folder holds the repo-relative path, or "repo" when none does.
func scopeOfPath(cfg *config.Config, path string) string {
	best := ""
	for _, h := range hostNames(cfg) {
		dir := filepath.ToSlash(cfg.Hosts[h].ResolvedDir(h))
		if (path == dir || strings.HasPrefix(path, dir+"/")) && len(dir) > 0 && (best == "" || len(dir) > len(cfg.Hosts[best].ResolvedDir(best))) {
			best = h
		}
	}
	if best == "" {
		return repoScope
	}
	return best
}

// authScope is where the auth provider's findings live: the host running the auth service, or "repo" without one.
func authScope(cfg *config.Config) string {
	if s, ok := cfg.Services[cfg.Defaults.AuthService]; ok && s.Host != "" {
		return s.Host
	}
	return repoScope
}

// remoteResult is what one remote host's doctor said.
type remoteResult struct {
	host string
	fs   []Finding
}

// sameFinding is whether two findings say the same thing: everything a person reads, not who reported it.
func sameFinding(a, b Finding) bool {
	return a.Status == b.Status && a.Title == b.Title && a.Summary == b.Summary &&
		reflect.DeepEqual(a.Notes, b.Notes) && reflect.DeepEqual(a.List, b.List) && reflect.DeepEqual(a.Steps, b.Steps)
}

// mergeFleet puts the deploy host's findings and each remote host's into one list, showing a problem once. Two findings are one
// when they have the same current-scheme ID AND say the same thing; they then become one finding naming every host that reported
// it. When they differ — a hand edit or a stale checkout on one host — each stays its own finding, so the difference is visible,
// and the later one gets its host appended to its ID so the two never share a key. An ID outside the scheme is never merged.
//
// Only the deploy host's findings can be fixed from here (the UI fixes the checkout it shows): a remote finding is never
// Fixable and, when it would have been, says to run the fix on that host.
func mergeFleet(self string, local []Finding, remotes []remoteResult) []Finding {
	remotes = append([]remoteResult(nil), remotes...)
	sort.SliceStable(remotes, func(i, j int) bool { return remotes[i].host < remotes[j].host }) // whichever answered first
	fs := make([]Finding, 0, len(local))
	at := map[string][]int{} // current-scheme ID as reported -> where its cards are in fs
	taken := map[string]bool{}
	for _, f := range local {
		f.Host, f.Hosts = self, []string{self}
		fs = append(fs, f)
		at[f.ID] = append(at[f.ID], len(fs)-1)
		taken[f.ID] = true
	}
	for _, r := range remotes {
		for _, f := range r.fs {
			if currentID(f.ID) {
				merged := false
				for _, i := range at[f.ID] {
					if sameFinding(fs[i], f) {
						fs[i].Hosts = append(fs[i].Hosts, r.host)
						merged = true
						break
					}
				}
				if merged {
					continue
				}
			}
			reported := f.ID
			switch {
			case !currentID(f.ID):
				f.ID = r.host + ":" + f.ID // an older hemma's ID: only ever unique per host
			case taken[f.ID]:
				f.ID += "@" + r.host // the same problem, said differently: its own card with its own key
			}
			if f.Status != "ok" && f.Status != "info" && (f.Fixable || len(f.Steps) == 0) {
				f.Steps = append(f.Steps, FindingStep{Label: "fix", Text: "run 'hemma doctor --fix' on " + r.host})
			}
			f.Fixable = false // the UI fixes the checkout it shows, which is this host's
			f.Host, f.Hosts = r.host, []string{r.host}
			fs = append(fs, f)
			if currentID(reported) {
				at[reported] = append(at[reported], len(fs)-1)
			}
			taken[f.ID] = true
		}
	}
	return fs
}
