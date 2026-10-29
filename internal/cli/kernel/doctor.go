package kernel

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hemma/internal/auth"
	"hemma/internal/config"
	"hemma/internal/plan"
	"hemma/internal/render"
	syncpkg "hemma/internal/sync"
)

// doctor is three steps, each its own piece of code:
//
//	diagnose  read-only: what is wrong (and what is fine), as Findings
//	fix       only with --fix: every fixable Finding runs the fixer it carries, then the repo is diagnosed again
//	print     the CLI's rendering of Findings as text (the UI draws the same Findings itself, from Diagnose)
//
// A Finding is fixable exactly when it carries a fixer, so what the UI offers to fix automatically and what `--fix`
// does cannot drift apart (TestIssues_FixableMatchesWhatFixDoes holds them to it).

// FindingStep is one labelled instruction of a finding: "fix" (what to change by hand) or "then" (the command to run after).
type FindingStep struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Finding is one result of a doctor check. The exported fields are what a UI shows; the unexported ones are how the CLI
// prints it and what makes it count.
type Finding struct {
	ID      string        `json:"id"`
	Status  string        `json:"status"` // ok | warn | err | info
	Title   string        `json:"title"`
	Summary string        `json:"summary,omitempty"`
	Notes   []string      `json:"notes,omitempty"`
	List    []string      `json:"list,omitempty"` // the files, rules or entries it is about
	Steps   []FindingStep `json:"steps,omitempty"`
	Fixable bool          `json:"fixable,omitempty"` // `hemma doctor --fix` repairs it: it carries a fixer

	headline string   // the first printed line, after the glyph; the Title when empty
	lines    []string // the printed lines under it, indentation included
	noGlyph  bool
	toStderr bool // printed as "Error: …" on stderr
	advisory bool // an auth advisory: a blank line before it, and the closing note after the last one
	counts   bool // makes doctor exit non-zero while it is not ok

	fix func(env *Env) ([]string, error) // what --fix does for it: the lines describing what it did
}

// withFix makes the finding fixable by giving it its fixer.
func (f Finding) withFix(fn func(env *Env) ([]string, error)) Finding {
	f.fix, f.Fixable = fn, true
	return f
}

// Problems is how many findings keep doctor from exiting 0.
func Problems(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if f.counts && f.Status != "ok" {
			n++
		}
	}
	return n
}

// Diagnose is the read-only half of doctor, for the UI: the same findings `hemma doctor` prints. It changes nothing.
func Diagnose(env *Env, cfgPath string) ([]Finding, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	return diagnose(env, filepath.Dir(cfgPath), cfg), nil
}

// diagnose runs every check, in the order doctor reports them.
func diagnose(env *Env, repoRoot string, cfg *config.Config) []Finding {
	var fs []Finding
	// Load the auth snippet source so the plan below renders the real (auth) file content, not the empty stub. On failure keep
	// the last good one — the way runSync does — but count it: a bad auth_snippet path is exactly what doctor exists to surface.
	if err := cfg.LoadAuthSnippet(repoRoot); err != nil {
		fs = append(fs, Finding{ID: "auth.snippet", Status: "err", counts: true, toStderr: true,
			Title:    "auth_snippet unreadable — keeping the existing generated auth snippet",
			Summary:  err.Error(),
			headline: fmt.Sprintf("auth_snippet unreadable — keeping the existing generated auth snippet: %v", err)})
	}
	p := plan.Build(cfg)
	fs = append(fs, diagGitignore(env, repoRoot, p)...)
	fs = append(fs, diagManagedBlock(filepath.Join(repoRoot, ".gitignore"))...)
	fs = append(fs, diagCaddyImports(repoRoot, cfg)...)
	fs = append(fs, diagTrustedProxies(repoRoot, cfg)...)
	fs = append(fs, diagDrift(env, repoRoot, cfg)...)
	fs = append(fs, diagDeployReadiness(env, cfg)...)
	fs = append(fs, diagAuth(repoRoot, cfg)...)
	return fs
}

// cmdDoctor audits the repo for problems hemma can detect. Read-only by default; `hemma doctor --fix` runs the fixer of every
// fixable finding (the .gitignore rules, the Caddyfile imports, the generated files), diagnoses again, and reports what is
// left. Exits non-zero if problems remain.
func cmdDoctor(env *Env, cfgPath string, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fixFlag := fs.Bool("fix", false, "apply fixes (write .gitignore entries)")
	fs.BoolVar(fixFlag, "f", false, "alias for --fix")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	repoRoot := filepath.Dir(cfgPath)
	cfg, code := loadExisting(env, cfgPath, "check")
	if cfg == nil {
		return code
	}
	findings := diagnose(env, repoRoot, cfg)
	if *fixFlag {
		if log := runFixers(env, findings); len(log) > 0 {
			for _, l := range log {
				fmt.Fprintln(env.Out, l)
			}
			fmt.Fprintln(env.Out)
		}
		findings = diagnose(env, repoRoot, cfg) // what is left, and what is now fine
	}
	printFindings(env, repoRoot, findings)
	if Problems(findings) > 0 {
		return 1
	}
	return 0
}

// runFixers runs the fixer of each fixable finding, in the order doctor reports them, and returns what happened as printable
// lines. A fixer that fails does not stop the others; the finding simply remains in the next diagnosis.
func runFixers(env *Env, fs []Finding) []string {
	var log []string
	for _, f := range fs {
		if f.fix == nil || f.Status == "ok" {
			continue
		}
		done, err := f.fix(env)
		for _, d := range done {
			log = append(log, tick+" "+d)
		}
		if err != nil {
			log = append(log, fmt.Sprintf("%s Could not fix %q: %v", cross, f.Title, err))
		}
	}
	return log
}

// printFindings is the CLI's rendering: a glyph and a headline per finding, then its lines.
func printFindings(env *Env, repoRoot string, fs []Finding) {
	advisories := 0
	for _, f := range fs {
		if f.advisory {
			fmt.Fprintln(env.Out)
			advisories++
		}
		head := f.headline
		if head == "" {
			head = f.Title
		}
		switch {
		case f.toStderr:
			errf(env, "%s", head)
		case f.noGlyph || f.Status == "info":
			fmt.Fprintln(env.Out, head)
		default:
			fmt.Fprintln(env.Out, glyphOf(f.Status)+" "+head)
		}
		for _, l := range f.lines {
			fmt.Fprintln(env.Out, l)
		}
	}
	if advisories > 0 {
		fmt.Fprintf(env.Out, "\n%sThese advisories require manual edits (see their fix: lines) — 'hemma doctor --fix' does not resolve them.%s\n", dimOn, dimOff)
	}
}

func glyphOf(status string) string {
	switch status {
	case "ok":
		return tick
	case "warn":
		return warn
	case "err":
		return cross
	}
	return ""
}

// ── .gitignore ──────────────────────────────────────────────────────────────────────────────────────────────

// diagGitignore: are any generated files ignored by git (they would be generated and never committed or deployed)?
func diagGitignore(env *Env, repoRoot string, p *plan.Plan) []Finding {
	ignored, ok := ignoredPaths(env, repoRoot, planPaths(p))
	switch {
	case !ok:
		return []Finding{{ID: "gitignore", Status: "info", Title: "Skipped gitignore check (git not available or not a repository)."}}
	case len(ignored) == 0:
		return []Finding{{ID: "gitignore", Status: "ok", Title: "No generated files are gitignored."}}
	}
	title := fmt.Sprintf("%d generated %s ignored by git — they won't be committed or deployed", len(ignored), plural(len(ignored), "file"))
	lines := []string{}
	for _, path := range ignored {
		lines = append(lines, "  "+path)
	}
	lines = append(lines, "", "Add to .gitignore to un-ignore them (or run 'hemma doctor --fix'):")
	for _, rule := range unignoreRules() {
		lines = append(lines, "  "+rule)
	}
	lines = append(lines, "", "Run 'hemma doctor --fix' to add these entries automatically.")
	return []Finding{Finding{ID: "gitignore", Status: "warn", Title: title, List: ignored, counts: true, noGlyph: true,
		headline: title + ":", lines: lines}.withFix(func(env *Env) ([]string, error) {
		if err := writeManagedBlock(filepath.Join(repoRoot, ".gitignore"), unignoreRules()); err != nil {
			return nil, err
		}
		done := []string{"Updated .gitignore"}
		if still, ok := ignoredPaths(env, repoRoot, planPaths(p)); ok && len(still) > 0 {
			return done, fmt.Errorf("%d %s still gitignored after the fix: %s", len(still), plural(len(still), "file"), strings.Join(still, ", "))
		}
		return done, nil
	})}
}

// diagManagedBlock keeps hemma's own .gitignore block current. When the block exists but is not what hemma would write now
// (an older hemma wrote broader rules) it is out of date, and --fix rewrites it in place — this is not about ignored files, so
// it also reaches repos whose generated files are already tracked. Blanket rules left OUTSIDE the block (the predecessor
// tool's) are only advised about: hemma does not edit what it does not own, so that advisory does not count.
func diagManagedBlock(giPath string) []Finding {
	b, err := os.ReadFile(giPath)
	if err != nil {
		return nil
	}
	content := string(b)
	have, found := managedBlockRules(content)
	want := unignoreRules()
	var fs []Finding
	if found && strings.Join(have, "\n") != strings.Join(want, "\n") {
		fs = append(fs, Finding{ID: "gitignore.block", Status: "warn", counts: true,
			Title:    "hemma's .gitignore block is out of date",
			Summary:  "this version of hemma writes narrower rules and no longer re-includes whole data/ directories.",
			headline: "hemma's .gitignore block is out of date: this version of hemma writes narrower rules and no longer re-includes whole data/ directories.",
			lines:    []string{"  Run 'hemma doctor --fix' to rewrite it."},
		}.withFix(func(env *Env) ([]string, error) {
			// the gitignore fixer may have written the block already: only write what is still out of date
			cur, err := os.ReadFile(giPath)
			if err != nil {
				return nil, err
			}
			if now, ok := managedBlockRules(string(cur)); ok && strings.Join(now, "\n") == strings.Join(want, "\n") {
				return nil, nil
			}
			if err := writeManagedBlock(giPath, want); err != nil {
				return nil, err
			}
			return []string{"Updated hemma's .gitignore block to the current (narrow) rules."}, nil
		}))
	} else if found {
		fs = append(fs, Finding{ID: "gitignore.block", Status: "ok", Title: ".gitignore block is current."})
	}
	// Blanket rules outside hemma's block.
	if s, e := strings.Index(content, giBlockStart), strings.Index(content, giBlockEnd); s >= 0 && e > s {
		content = content[:s] + content[e+len(giBlockEnd):]
	}
	var stray []string
	for _, line := range strings.Split(content, "\n") {
		if l := strings.TrimSpace(line); blanketDataRules[l] {
			stray = append(stray, l)
		}
	}
	if len(stray) > 0 {
		rules := strings.Join(dedupe(stray), ", ")
		fs = append(fs, Finding{ID: "gitignore.blanket", Status: "warn",
			Title:    fmt.Sprintf(".gitignore has blanket data/ rules outside hemma's block (%s)", rules),
			Summary:  "they make git walk all runtime data on every 'git status'. Remove them — hemma's narrow rules replace them.",
			headline: fmt.Sprintf(".gitignore has blanket data/ rules outside hemma's block (%s): they make git walk all runtime data on every 'git status'. Remove them — hemma's narrow rules replace them.", rules)})
	}
	return fs
}

// ── Caddyfiles ──────────────────────────────────────────────────────────────────────────────────────────────

// hostNames is the configured hosts in a fixed order, so findings (and their printing) do not depend on map iteration.
func hostNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Hosts))
	for n := range cfg.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func caddyfilePath(repoRoot string, cfg *config.Config, host string) string {
	return filepath.Join(repoRoot, cfg.Hosts[host].ResolvedDir(host), config.DefaultCaddyDataDir, "Caddyfile")
}

// diagCaddyImports checks that each host's Caddyfile contains the line that pulls in hemma's generated sites. An outdated
// import line (an older name of the tool) is rewritten by --fix, a missing one is appended.
func diagCaddyImports(repoRoot string, cfg *config.Config) []Finding {
	const importLine = "import " + render.CaddyImportFilename
	legacyImportLines := []string{"import splitdns.generated.caddy", "import sd.generated.caddy"}
	var fs []Finding
	for _, host := range hostNames(cfg) {
		cfPath := caddyfilePath(repoRoot, cfg, host)
		id := "caddy.import:" + host
		data, err := os.ReadFile(cfPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fs = append(fs, Finding{ID: id, Status: "err", counts: true, Title: fmt.Sprintf("%s: cannot read Caddyfile: %v", host, err)})
			continue
		}
		if strings.Contains(string(data), importLine) {
			fs = append(fs, Finding{ID: id, Status: "ok", Title: fmt.Sprintf("%s: Caddyfile imports %s", host, render.CaddyImportFilename)})
			continue
		}
		var legacy string
		for _, l := range legacyImportLines {
			if strings.Contains(string(data), l) {
				legacy = l
				break
			}
		}
		// Both are what --fix repairs, so both say so: the hint is how a person reading the text knows.
		f := Finding{ID: id, Status: "err", counts: true}
		if legacy != "" {
			f.Title = fmt.Sprintf("%s: Caddyfile has outdated `%s`", host, legacy)
			f.lines = []string{"  Run 'hemma doctor --fix' to rewrite it."}
		} else {
			f.Title = fmt.Sprintf("%s: Caddyfile missing `%s`", host, importLine)
			f.lines = []string{"  Run 'hemma doctor --fix' to add it."}
		}
		host, legacy := host, legacy
		fs = append(fs, f.withFix(func(env *Env) ([]string, error) {
			cur, err := os.ReadFile(cfPath)
			if err != nil {
				return nil, err
			}
			content := string(cur)
			if strings.Contains(content, importLine) {
				return nil, nil
			}
			var done string
			if legacy != "" {
				content = strings.ReplaceAll(content, legacy, importLine)
				done = fmt.Sprintf("%s: rewrote `%s` -> `%s`", host, legacy, importLine)
			} else {
				if !strings.HasSuffix(content, "\n") {
					content += "\n"
				}
				content += "\n" + importLine + "\n"
				done = fmt.Sprintf("%s: added `%s` to Caddyfile", host, importLine)
			}
			if err := config.AtomicWrite(cfPath, []byte(content)); err != nil {
				return nil, fmt.Errorf("%s: %w", host, err)
			}
			return []string{done}, nil
		}))
	}
	return fs
}

// diagTrustedProxies checks that each host's Caddyfile contains the line `import hemma.trusted_proxies.generated.caddy`.
// Unlike diagCaddyImports this is REPORT-ONLY — --fix never edits the Caddyfile for it. The import must sit inside a global
// `servers {}` options block (Caddy's trusted_proxies option is server-wide only, never valid in a site block), Caddy allows
// exactly one global options block per file, and that block is hand-maintained territory (it may already carry `debug` or
// anything else). Writing into it would mean parsing and rewriting Caddyfile syntax hemma does not own — and guessing wrong
// appends a second global block, which Caddy refuses to load. A one-time manual line per host is the cheaper, safer contract,
// so this check states what to add and stops there (an instructive advisory in the §6.4 two-tier sense, not a mechanical
// fix). It does not count toward the exit code: that would make `doctor --fix` permanently unable to exit 0. Enforcement
// lives in `apply`, which hard-refuses rather than reload a Caddy whose generated trust boundary is never loaded (§4.7).
func diagTrustedProxies(repoRoot string, cfg *config.Config) []Finding {
	const importLine = "import " + render.TrustedProxiesFilename
	var fs []Finding
	for _, host := range hostNames(cfg) {
		data, err := os.ReadFile(caddyfilePath(repoRoot, cfg, host))
		id := "caddy.trusted:" + host
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fs = append(fs, Finding{ID: id, Status: "err", Title: fmt.Sprintf("%s: cannot read Caddyfile: %v", host, err)})
			continue
		}
		if strings.Contains(string(data), importLine) {
			fs = append(fs, Finding{ID: id, Status: "ok", Title: fmt.Sprintf("%s: Caddyfile imports %s", host, render.TrustedProxiesFilename)})
			continue
		}
		fs = append(fs, Finding{ID: id, Status: "err",
			Title:   fmt.Sprintf("%s: Caddyfile missing `%s`", host, importLine),
			Summary: "add it inside your global options block (hemma does not edit that block):",
			List:    []string{"{", "servers {", importLine, "}", "}"},
			lines: []string{
				"    add it inside your global options block (hemma does not edit that block):",
				"      {", "          servers {", "              " + importLine, "          }", "      }",
			}})
	}
	return fs
}

// ── generated files ─────────────────────────────────────────────────────────────────────────────────────────

// driftEntries lists each drifted file grouped by class, as "missing:  path" and so on.
func driftEntries(d Drift) []string {
	var out []string
	for _, f := range d.Missing {
		out = append(out, "missing:  "+f)
	}
	for _, f := range d.Modified {
		out = append(out, "modified: "+f)
	}
	for _, f := range d.Orphaned {
		out = append(out, "orphaned: "+f)
	}
	return out
}

// diagDrift compares the generated files on disk with what services.yaml would generate. The repair is a full Complete
// reconcile: it rewrites missing and modified files and GCs orphaned tracked files.
func diagDrift(env *Env, repoRoot string, cfg *config.Config) []Finding {
	mf := loadManifest(env, repoRoot, cfg)
	d := detectDrift(repoRoot, cfg, mf)
	if !d.Any() {
		return []Finding{{ID: "drift", Status: "ok", Title: "Generated files are in sync with services.yaml."}}
	}
	title := fmt.Sprintf("%d generated %s out of sync with services.yaml", d.Count(), plural(d.Count(), "file"))
	entries := driftEntries(d)
	lines := []string{}
	for _, e := range entries {
		lines = append(lines, "  - "+e)
	}
	lines = append(lines, "", "Run 'hemma doctor --fix' to reconcile (regenerate missing/modified, remove orphaned).")
	return []Finding{Finding{ID: "drift", Status: "err", Title: title, List: entries, counts: true, headline: title + ":", lines: lines}.withFix(func(env *Env) ([]string, error) {
		eng := &syncpkg.Engine{RepoRoot: repoRoot, Manifest: loadManifest(env, repoRoot, cfg)}
		res, err := eng.Reconcile(plan.Build(cfg), syncpkg.Complete)
		if err != nil {
			return nil, err
		}
		n := len(res.Created) + len(res.Updated) + len(res.Deleted)
		return []string{fmt.Sprintf("Reconciled %d generated %s (%d created, %d updated, %d removed).",
			n, plural(n, "file"), len(res.Created), len(res.Updated), len(res.Deleted))}, nil
	})}
}

// ── deploy readiness ────────────────────────────────────────────────────────────────────────────────────────

// diagDeployReadiness audits, LOCALLY, whether `hemma deploy` could reach every remote target from this machine: the host key
// must already be in known_hosts. deploy sshes with BatchMode=yes, which cannot answer the interactive trust prompt, so an
// unknown host key fails deploy outright. Checked with `ssh-keygen -F` against both the host's ip and its ssh destination
// (either satisfies ssh's lookup); -F understands hashed entries where a grep would not.
//
// This check never connects — doctor stays inside the no-SSH boundary (design §12; deploy is the ONE exception, and its
// phase-0 probe is where "can we actually connect" is verified for real). It is not --fix-able: hemma does not own
// known_hosts, so the recipe is to connect once interactively, so a human sees and accepts the fingerprint (deliberately NOT
// ssh-keyscan, which would trust whatever key is on the wire unseen).
//
// The target set mirrors deploy's default (every host with a role); self is skipped (deploy runs locally there, no ssh).
func diagDeployReadiness(env *Env, cfg *config.Config) []Finding {
	self := localHost(cfg)
	// Readiness is a question about the deploy ORIGIN, not about every host. When defaults.deploy_host names one, only that host
	// is audited: elsewhere the finding would be unfixable by design (a segmented network may give a non-origin host no route to
	// its peers at all), and an unfixable finding that fails doctor forever teaches people to ignore doctor. Unset keeps the old
	// behaviour of checking everywhere, so adopting the field is opt-in.
	if dh := cfg.Defaults.DeployHost; dh != "" && self != dh {
		return nil
	}
	targets, err := resolveDeployTargets(cfg, self, nil)
	if err != nil {
		return nil // config-level problems are reported elsewhere
	}
	var fs []Finding
	remotes, problems := 0, 0
	for _, t := range targets {
		if t.Local {
			continue
		}
		remotes++
		ip := cfg.Hosts[t.Name].IP
		if hostKnown(env, ip) || hostKnown(env, sshHostPart(t.Dest)) {
			continue
		}
		problems++
		title := fmt.Sprintf("%s: no known_hosts entry for its deploy destination (%s)", t.Name, t.Dest)
		fs = append(fs, Finding{ID: "deploy.known_hosts:" + t.Name, Status: "warn", counts: true, Title: title,
			Summary: "'hemma deploy' uses 'ssh -o BatchMode=yes' and will fail host-key verification.",
			Steps:   []FindingStep{{Label: "fix", Text: fmt.Sprintf("connect once interactively to accept and record the host key — ssh %s (verify the fingerprint before accepting.)", t.Dest)}},
			lines: []string{
				"  'hemma deploy' uses 'ssh -o BatchMode=yes' and will fail host-key verification.",
				"  fix: connect once interactively to accept and record the host key —",
				"       ssh " + t.Dest,
				"       (verify the fingerprint before accepting.)",
			}})
	}
	if remotes > 0 && problems == 0 {
		fs = append(fs, Finding{ID: "deploy.known_hosts", Status: "ok", Title: fmt.Sprintf("Deploy readiness: %d remote %s in known_hosts.", remotes, plural(remotes, "host"))})
	}
	return fs
}

// ── auth ────────────────────────────────────────────────────────────────────────────────────────────────────

// diagAuth turns the auth provider's read-only checks (config, users database, wiring) into findings. Report-but-proceed: none
// of them corrupts generated files and none is --fix-able (hemma never writes docker-compose.yml, configuration.yml or the users
// database), so each carries the full paste-in recipe. Only snippet-without-auth_service counts: it reproduces the redirect-loop
// bug.
func diagAuth(repoRoot string, cfg *config.Config) []Finding {
	var fs []Finding
	cfgAdvs := authConfigWarnings(repoRoot, cfg)
	loop := cfg.Defaults.AuthSnippet != "" && cfg.Defaults.AuthService == ""
	if len(cfgAdvs) == 0 && cfg.Defaults.AuthSnippet != "" {
		fs = append(fs, Finding{ID: "auth.consistent", Status: "ok", Title: "Auth config is consistent."})
	}
	advs := append(append(cfgAdvs, usersDBWarnings(repoRoot, cfg)...), authWiringWarnings(repoRoot, cfg)...)
	for i, a := range advs {
		f := advisoryFinding(repoRoot, a)
		f.ID = fmt.Sprintf("auth.advisory:%d", i+1)
		f.counts = loop && i < len(cfgAdvs)
		fs = append(fs, f)
	}
	return fs
}

// advisoryFinding turns an auth.Advisory into a Finding: the structure for a UI, and the exact lines the CLI prints.
func advisoryFinding(repoRoot string, a auth.Advisory) Finding {
	head, lines := advisoryText(repoRoot, a)
	rel := func(s string) string { return strings.ReplaceAll(s, repoRoot+string(filepath.Separator), "") }
	f := Finding{Status: "warn", advisory: true, Title: head, headline: head, lines: lines}
	for i, b := range a.Body {
		if i == 0 {
			f.Summary = rel(b)
		} else {
			f.Notes = append(f.Notes, rel(b))
		}
	}
	if len(a.Fix) > 0 {
		parts := make([]string, len(a.Fix))
		for i, s := range a.Fix {
			parts[i] = strings.TrimSpace(rel(s))
		}
		f.Steps = append(f.Steps, FindingStep{Label: "fix", Text: strings.Join(parts, " ")})
	}
	if a.Then != "" {
		f.Steps = append(f.Steps, FindingStep{Label: "then", Text: a.Then})
	}
	return f
}
