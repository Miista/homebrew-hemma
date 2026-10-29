package kernel

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/plan"
	"hemma/internal/render"
)

// planPaths returns the unique repo-relative paths a plan would write.
func planPaths(p *plan.Plan) []string {
	var paths []string
	seen := map[string]bool{}
	for _, files := range p.Files {
		for _, f := range files {
			if !seen[f.Path] {
				seen[f.Path] = true
				paths = append(paths, f.Path)
			}
		}
	}
	return paths
}

// warnIfIgnored prints a SHORT one-line warning to stderr if any of the plan's
// output paths are gitignored (they'd generate but never commit/deploy). It
// fires every run while the problem persists — a standing deploy hazard should
// stay visible — and points at `hemma doctor` for the full file list + fix.
// Silent when nothing is ignored or the check can't run (git absent / no repo).
func warnIfIgnored(env *Env, repoRoot string, p *plan.Plan) {
	ignored, ok := ignoredPaths(env, repoRoot, planPaths(p))
	if !ok || len(ignored) == 0 {
		return
	}
	noun := plural(len(ignored), "file")
	verb := "is"
	if len(ignored) != 1 {
		verb = "are"
	}
	fmt.Fprintf(env.Err,
		warn+" %d generated %s %s gitignored and won't deploy. Run 'hemma doctor --fix'.\n",
		len(ignored), noun, verb)
}

// authWiringWarnings runs the provider's read-only access-control wiring
// check for `doctor`: is the generated artifact (§4.6) actually loaded by the
// auth container per the auth host's docker-compose.yml? Mirrors
// usersDBWarnings' config-location logic; silent when the auth service or its
// host isn't resolvable (flagged elsewhere), when the auth service is
// disabled (apply skips its auth half too), or when the artifact isn't part
// of the plan (gated inside the provider).
func authWiringWarnings(repoRoot string, cfg *config.Config) []auth.Advisory {
	if cfg.Defaults.AuthService == "" {
		return nil
	}
	authSvc, ok := cfg.Services[cfg.Defaults.AuthService]
	if !ok || authSvc.Disabled {
		return nil
	}
	hostM, ok := cfg.Hosts[authSvc.Host]
	if !ok {
		return nil
	}
	hostDir := filepath.Join(repoRoot, hostM.ResolvedDir(authSvc.Host))
	var svcs []auth.Service
	for name, s := range cfg.Services {
		if !s.Auth.Mode.UsesProvider() || s.Disabled {
			continue
		}
		svcs = append(svcs, auth.Service{Name: name, FQDN: s.FQDN, Mode: string(s.Auth.Mode), Groups: s.Auth.Groups, BypassPaths: s.Auth.BypassPaths,
			ForwardGate: s.Auth.Mode == config.AuthOIDC && s.Auth.ForwardGate})
	}
	return auth.Default().ValidateWiring(hostDir, cfg.Defaults.AuthService, svcs)
}

const (
	giBlockStart = "# >>> hemma managed >>>"
	giBlockEnd   = "# <<< hemma managed <<<"
)

// Pre-rename marker generations (splitdns era, sd era); rewritten to the new
// ones on the next write so the old block is replaced in place instead of a
// duplicate being appended.
var legacyGiBlockStarts = []string{"# >>> splitdns managed >>>", "# >>> sd managed >>>"}
var legacyGiBlockEnds = []string{"# <<< splitdns managed <<<", "# <<< sd managed <<<"}

// writeManagedBlock writes rules into path inside a marked hemma block,
// creating the file if absent and preserving any content outside the markers.
// Idempotent.
func writeManagedBlock(path string, rules []string) error {
	var existing string
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Migrate pre-rename markers so the block below replaces the old block.
	for _, m := range legacyGiBlockStarts {
		existing = strings.ReplaceAll(existing, m, giBlockStart)
	}
	for _, m := range legacyGiBlockEnds {
		existing = strings.ReplaceAll(existing, m, giBlockEnd)
	}

	block := giBlockStart + "\n" +
		"# hemma-generated config under data/ dirs the repo otherwise ignores.\n" +
		"# Only the directories hemma writes into are re-included; the rest of every data/ stays\n" +
		"# ignored and is never walked by git. Managed by 'hemma doctor --fix'; edit outside these markers.\n" +
		strings.Join(rules, "\n") + "\n" +
		giBlockEnd + "\n"

	var out string
	if s, e := strings.Index(existing, giBlockStart), strings.Index(existing, giBlockEnd); s >= 0 && e > s {
		// Replace the existing block, preserving everything around it.
		tail := existing[e+len(giBlockEnd):]
		tail = strings.TrimPrefix(tail, "\n")
		out = existing[:s] + block + tail
	} else if existing == "" {
		out = block
	} else {
		// Append, ensuring a separating newline.
		if !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		out = existing + "\n" + block
	}
	return config.AtomicWrite(path, []byte(out))
}

// managedBlockRules returns the rule lines inside hemma's managed block of a
// .gitignore (comments and blanks dropped), and whether the block exists.
func managedBlockRules(content string) (rules []string, found bool) {
	for _, m := range legacyGiBlockStarts {
		content = strings.ReplaceAll(content, m, giBlockStart)
	}
	for _, m := range legacyGiBlockEnds {
		content = strings.ReplaceAll(content, m, giBlockEnd)
	}
	s, e := strings.Index(content, giBlockStart), strings.Index(content, giBlockEnd)
	if s < 0 || e <= s {
		return nil, false
	}
	for _, line := range strings.Split(content[s+len(giBlockStart):e], "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	return rules, true
}

// blanketDataRules are the lines older hemma versions (and its predecessor) wrote:
// they un-ignore every directory under every data/, so git walks all runtime data on
// every `git status`. The current rules never contain them.
var blanketDataRules = map[string]bool{"!**/data/": true, "!**/data/**/": true}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ignoredPaths returns the subset of repo-relative paths that git would ignore
// in repoRoot, using git's own logic (git check-ignore). It returns (nil, false)
// when the check can't run — git missing, or repoRoot isn't a work tree — so
// callers can skip the warning rather than guess.
func ignoredPaths(env *Env, repoRoot string, relPaths []string) (ignored []string, ok bool) {
	if len(relPaths) == 0 {
		return nil, true
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	// `git check-ignore --stdin` prints the paths it would ignore, one per line.
	cmd := env.command("git", "-C", repoRoot, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(relPaths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 = "nothing ignored" (not an error for us). Any other
		// failure (not a repo, etc.) → can't determine; signal not-ok.
		if ee, isExit := err.(*exec.ExitError); isExit && ee.ExitCode() == 1 {
			return nil, true
		}
		return nil, false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			ignored = append(ignored, s)
		}
	}
	sort.Strings(ignored)
	return ignored, true
}

// unignoreRules returns the repo-root .gitignore negation block that
// re-includes hemma's generated files when a broad rule like **/data/** would
// otherwise ignore them.
//
// Git won't re-include a file under an excluded directory, so each directory
// between a data/ dir and a generated file is un-ignored first, then only the
// generated files in it are re-included — runtime data (.db, caches, …) stays
// ignored. The rules are deliberately NARROW: a blanket "!**/data/**/" would
// un-ignore every directory under every data/, and git would then walk all of
// the runtime data on every `git status` (thousands of files, seconds on a Pi,
// and `git status` is what the web UI runs on each page load). Here git only
// descends the few directories hemma writes into and skips the rest.
//
// The block is FIXED, not derived from what the current config happens to
// generate: it is the same in every repo and at every moment, so it covers a
// kind of output from the start (before the first service, or the first auth
// service, exists), never changes with the config, and needs no write unless
// hemma itself changes. It is built from the same constants the plan uses for
// its output paths, plus each registered auth provider's own config directory
// (where its generated artifact sits); TestUnignoreRules_CoversEveryPlanOutput
// fails if hemma ever writes under a data/ directory these rules do not cover.
func unignoreRules() []string {
	// One representative file per kind of output. Only the directory and the
	// file's marker (".generated.<ext>", or the extension) matter.
	shapes := []string{
		path.Join(config.DefaultCaddyDataDir, render.CaddyImportFilename),
		path.Join(config.DefaultCaddySitesDir, "service.caddy"),
		path.Join(config.DefaultCaddyTLSDir, render.TLSSnippetName("domain")+".caddy"),
		path.Join(config.DefaultDnsmasqDir, "service.generated.conf"),
	}
	for _, name := range auth.Names() {
		if prov, ok := auth.Lookup(name); ok {
			shapes = append(shapes, path.Join(path.Dir(prov.ConfigPath()), "artifact.generated.yml"))
		}
	}
	return rulesForOutputs(shapes)
}

// rulesForOutputs turns output file paths into narrow, host-agnostic negation
// rules: described from the service directory in front of data/ (caddy, pihole,
// authelia) onward, so one block covers every host. A file is matched by what
// marks it as generated — its ".generated.<ext>" tail, or else (caddy site
// files carry no marker) its extension. Paths with no data/ segment need no
// rule. The result is sorted and deduplicated, so it is stable.
func rulesForOutputs(outputPaths []string) []string {
	set := map[string]bool{}
	for _, p := range outputPaths {
		segs := strings.Split(filepath.ToSlash(p), "/")
		di := -1
		for i, s := range segs {
			if s == "data" && i > 0 && i < len(segs)-1 {
				di = i
				break
			}
		}
		if di < 0 {
			continue
		}
		base := "!**/" + segs[di-1] + "/data/"
		file := segs[len(segs)-1]
		dirs := segs[di+1 : len(segs)-1]
		for k := 1; k <= len(dirs); k++ {
			set[base+strings.Join(dirs[:k], "/")+"/"] = true
		}
		match := file
		if i := strings.Index(file, ".generated."); i >= 0 {
			match = "*" + file[i:]
		} else if ext := filepath.Ext(file); ext != "" {
			match = "*" + ext
		}
		if len(dirs) > 0 {
			match = strings.Join(dirs, "/") + "/" + match
		}
		set[base+match] = true
	}
	rules := make([]string, 0, len(set))
	for r := range set {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	return rules
}
