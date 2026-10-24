package main

import (
	"regexp"
	"strings"
)

// The Issues page is a checklist: one item per check, the failing ones expandable, with a Fix button where
// `hemma doctor --fix` helps and the how-to text where it does not.
//
// hemma core is NOT changed for this. Doctor keeps printing text for people at a terminal, and this file reads that
// text into items — the same approach the page always took, now done in one tested place on the server instead of in
// the browser. It is tied to doctor's own wording, so tests run the REAL doctor over real situations
// (TestIssues_Items_FollowRealDoctor): a change to doctor's output breaks a test instead of quietly dropping an item
// or showing a Fix button that does nothing.

type issueStep struct {
	Label string `json:"label"` // fix | then | …
	Text  string `json:"text"`
}

type issueItem struct {
	Status  string      `json:"status"`            // ok | warn | err | info
	Title   string      `json:"title"`             // what is wrong (or what passed)
	Summary string      `json:"summary,omitempty"` // one sentence of why / what it means
	Notes   []string    `json:"notes,omitempty"`   // further sentences
	List    []string    `json:"list,omitempty"`    // the files, rules or entries it is about
	Steps   []issueStep `json:"steps,omitempty"`   // how to fix it by hand (fix: / then:)
	Fixable bool        `json:"fixable,omitempty"` // `hemma doctor --fix` resolves it
}

var (
	itemLabelRe   = regexp.MustCompile(`(?i)^(fix|then|run|hint|try|see)\s*:\s*(.*)$`)
	itemBulletRe  = regexp.MustCompile(`^[•*-]\s+(.*)$`)
	itemIgnoredRe = regexp.MustCompile(`^\d+ generated files? (is|are)? ?ignored by git`) // doctor prints this finding without a glyph
)

// doctor's closing note about advisories: the page shows each item's own how-to, so the note itself is dropped.
const doctorAdvisoryNote = "does not resolve them"

func parseDoctorItems(out string) []issueItem {
	type group struct {
		status string
		lines  []string
	}
	var groups []*group
	var cur *group
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			cur = nil
			continue
		}
		status := ""
		switch {
		case strings.HasPrefix(line, "✓"):
			status = "ok"
		case strings.HasPrefix(line, "⚠"):
			status = "warn"
		case strings.HasPrefix(line, "✗"), strings.HasPrefix(line, "Error:"):
			status = "err"
		}
		switch {
		case status != "":
			cur = &group{status, []string{line}}
			groups = append(groups, cur)
		case cur != nil && (line[0] == ' ' || line[0] == '\t'):
			cur.lines = append(cur.lines, line) // an indented continuation of the finding above it
		default: // not indented: doctor's own line, or a finding that has no glyph, even straight after another
			cur = &group{"note", []string{line}} // a line of doctor's own, between findings
			groups = append(groups, cur)
		}
	}

	items := []issueItem{}
	for _, g := range groups {
		if strings.Contains(strings.Join(g.lines, "\n"), doctorAdvisoryNote) {
			continue
		}
		status := g.status
		if status == "note" && itemIgnoredRe.MatchString(strings.TrimSpace(g.lines[0])) {
			status = "warn"
		}
		// A line of doctor's own that follows a finding ("Run 'hemma doctor --fix' to …") belongs to it.
		if status == "note" && len(items) > 0 && (items[len(items)-1].Status == "warn" || items[len(items)-1].Status == "err") {
			addItemLines(&items[len(items)-1], g.lines)
			continue
		}
		if status == "note" {
			status = "info"
		}
		it := issueItem{Status: status}
		first := strings.TrimSpace(strings.TrimLeft(g.lines[0], "✓⚠✗ "))
		it.Title = first
		// A long first sentence is the claim, then its explanation.
		if i := strings.Index(first, ": "); len(first) > 100 && i > 0 && i <= 90 {
			it.Title, it.Summary = first[:i], first[i+2:]
		}
		it.Title = strings.TrimSuffix(it.Title, ":")
		addItemLines(&it, g.lines[1:])
		items = append(items, it)
	}
	return items
}

// addItemLines reads the lines under a headline: labelled steps (fix: / then:), list items (bullets, or an indented
// single token such as a path or a rule), and prose. A hint that mentions `doctor --fix` marks the item fixable and is
// not shown — the Fix button replaces it — and the list that follows it ("Add to .gitignore: …") is the by-hand
// version of the same fix, so it is not shown either.
func addItemLines(it *issueItem, lines []string) {
	lastStep, skipList := -1, false
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if strings.Contains(t, "doctor --fix") && !strings.Contains(t, doctorAdvisoryNote) {
			it.Fixable, skipList, lastStep = true, true, -1
			continue
		}
		if m := itemLabelRe.FindStringSubmatch(t); m != nil {
			it.Steps = append(it.Steps, issueStep{Label: strings.ToLower(m[1]), Text: m[2]})
			lastStep, skipList = len(it.Steps)-1, false
			continue
		}
		bullet := itemBulletRe.FindStringSubmatch(t)
		token := len(raw)-len(strings.TrimLeft(raw, " ")) >= 2 && !strings.ContainsAny(t, " \t")
		if bullet != nil || token {
			if !skipList {
				if bullet != nil {
					t = bullet[1]
				}
				it.List = append(it.List, t)
			}
			lastStep = -1
			continue
		}
		skipList = false
		switch {
		case lastStep >= 0 && len(raw)-len(strings.TrimLeft(raw, " ")) >= 4: // a deeper-indented line continues the step above it
			it.Steps[lastStep].Text += " " + t
		case it.Summary == "":
			it.Summary, lastStep = t, -1
		default:
			it.Notes, lastStep = append(it.Notes, t), -1
		}
	}
}
