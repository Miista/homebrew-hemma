package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The registry below is the ONLY thing the UI can execute: an allowlist of
// hemma commands with typed fields. The browser form is generated from it
// (GET /api/commands) and the server builds the argv from it, so a command
// cannot exist in one place and not the other. Values are passed as an argv
// vector (never a shell), flags in --name=value form so a value can't be
// parsed as another flag, and positionals may not start with '-'.

type field struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`          // pos | str | list | bool | int | select
	Ref      string   `json:"ref,omitempty"` // what the value IS (host, service, cidr…): drives the picker + validation, see refs.go
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	Help     string   `json:"help,omitempty"`
	// Shows: the field is only offered while another field has one of these values, as
	// "<field>=<v1>|<v2>", several joined by "&" (all must hold; a bool is "true"/"false"). The browser hides it otherwise; checkAuthFields is the server's own check.
	Shows string `json:"shows,omitempty"`
	// Section: a heading the browser draws above this field and the ones after it, until the next Section.
	// Fields before the first heading are drawn ungrouped.
	Section string `json:"section,omitempty"`
	// Default: what a select shows (and an empty value means) when nothing is chosen yet, so it never needs a blank
	// entry that would read as a second spelling of the same thing.
	Default string `json:"default,omitempty"`
}

type command struct {
	Words      []string      `json:"-"`
	Key        string        `json:"key"`
	Group      string        `json:"group"`
	Title      string        `json:"title"`
	Fields     []field       `json:"fields"`
	Help       string        `json:"help,omitempty"`
	Update     bool          `json:"update,omitempty"`  // empty string values are sent (they clear)
	Confirm    string        `json:"confirm,omitempty"` // UI asks before running
	ReadOnly   bool          `json:"readonly,omitempty"`
	FlagsFirst bool          `json:"-"` // flags before positionals (list/verify/measure/...)
	Timeout    time.Duration `json:"-"`
}

var (
	authModes = []string{"none", "forward", "oidc", "external"} // "none" is the one spelling of "no auth"; a blank option would mean the same and read as a second one
	levels    = []string{"", "bypass", "one_factor", "two_factor", "deny"}
	posRe     = regexp.MustCompile(`^[^\s\x00-\x1f-][^\s\x00-\x1f]*$`)
)

func pos(name, label string, req bool) field {
	return field{Name: name, Label: label, Kind: "pos", Required: req}
}
func ref(f field, r string) field       { f.Ref = r; return f }
func withShows(f field, s string) field { f.Shows = s; return f }
func inSection(f field, s string) field { f.Section = s; return f }
func flag(name, label string) field     { return field{Name: name, Label: label, Kind: "bool"} }
func list(name, label, r string) field {
	return field{Name: name, Label: label, Kind: "list", Ref: r}
}

func serviceFields(add bool) []field {
	nameRef := "service" // updating: must already exist
	if add {
		nameRef = "svcname"
	}
	return []field{
		inSection(ref(pos("name", "Name", true), nameRef), "General"),
		{Name: "host", Label: "Host", Kind: "str", Ref: "host", Required: add},
		{Name: "backend", Label: "Backend (name:port)", Kind: "str", Ref: "backend", Required: add},
		inSection(field{Name: "fqdn", Label: "Address", Kind: "str", Ref: "fqdn", Required: add}, "Network"),
		{Name: "tunnel", Label: "Reachable from the internet", Kind: "bool", Help: "Routes through the cloudflared tunnel."},
		inSection(field{Name: "auth-mode", Label: "Auth mode", Kind: "select", Options: authModes, Default: "none"}, "Authentication"),
		// Only offered where they mean something (plan.go: groups and bypass paths need a mode that goes through
		// the auth provider; a per-horizon policy needs one too) — so the form cannot build a combination hemma skips.
		withShows(list("auth-groups", "Auth groups", "name"), "auth-mode=forward|oidc"),
		withShows(list("bypass-paths", "Bypass paths", "path"), "auth-mode=forward|oidc"),
		withShows(field{Name: "auth-policy-local", Label: "Policy: local horizon", Kind: "select", Options: levels}, "auth-mode=forward|oidc"),
		withShows(field{Name: "auth-policy-public", Label: "Policy: public horizon", Kind: "select", Options: levels}, "auth-mode=forward|oidc&tunnel=true"),
	}
}

var commands = []command{
	{Words: []string{"add", "service"}, Group: "Services", Title: "Add service", Fields: serviceFields(true), Timeout: 2 * time.Minute},
	{Words: []string{"update", "service"}, Group: "Services", Title: "Update service", Fields: serviceFields(false), Update: true, Timeout: 2 * time.Minute},
	{Words: []string{"remove", "service"}, Group: "Services", Title: "Remove service", Fields: []field{ref(pos("name", "Name", true), "service")}, Confirm: "Remove this service and its generated files?"},
	{Words: []string{"disable", "service"}, Group: "Services", Title: "Disable service", Fields: []field{ref(pos("name", "Name", true), "service")}},
	{Words: []string{"enable", "service"}, Group: "Services", Title: "Enable service", Fields: []field{ref(pos("name", "Name", true), "service")}},
	{Words: []string{"rename", "service"}, Group: "Services", Title: "Rename service", Fields: []field{ref(pos("old", "Current name", true), "service"), ref(pos("new", "New name", true), "svcname")}},

	{Words: []string{"add", "host"}, Group: "Hosts", Title: "Add host", Help: "A host's name is its directory in the repo, which must already exist.",
		Fields: []field{ref(pos("name", "Name", true), "svcname"), ref(pos("ip", "IP address", true), "ip"), {Name: "ssh", Label: "SSH destination (optional)", Kind: "str", Ref: "ssh"}, list("trusted-proxies", "Extra trusted proxies", "ipcidr")}},
	{Words: []string{"update", "host"}, Group: "Hosts", Title: "Update host", Update: true,
		Fields: []field{ref(pos("name", "Name", true), "host"), {Name: "ip", Label: "IP address", Kind: "str", Ref: "ip"}, {Name: "ssh", Label: "SSH destination (optional)", Kind: "str", Ref: "ssh"}, list("trusted-proxies", "Extra trusted proxies", "ipcidr")}},
	{Words: []string{"remove", "host"}, Group: "Hosts", Title: "Remove host", Fields: []field{ref(pos("name", "Name", true), "host")}, Confirm: "Remove this host?"},

	{Words: []string{"add", "domain"}, Group: "Domains", Title: "Add domain", Fields: []field{ref(pos("name", "Domain", true), "dnsname")}},
	{Words: []string{"remove", "domain"}, Group: "Domains", Title: "Remove domain", Fields: []field{ref(pos("name", "Domain", true), "domain")}, Confirm: "Remove this domain?"},

	{Words: []string{"defaults", "set"}, Group: "Defaults", Title: "Set default", Fields: []field{
		{Name: "key", Label: "Setting", Kind: "pos", Required: true, Options: []string{"dns-host", "deploy-host", "auth-snippet", "auth-service", "local-networks", "tunnel-dir"}},
		{Name: "value", Label: "Value ('-' clears)", Kind: "pos", Required: true}}},

	{Words: []string{"apply"}, Group: "Operate", Title: "Apply on this host", Timeout: 5 * time.Minute, Fields: nil,
		Confirm: "Apply now? This makes the saved config live on this host."},

	// deploy is the kernel's fleet deploy (design §6.8), exactly as on the CLI: ssh to every other host to
	// `git pull --ff-only` and apply, then this host last (its own apply runs in-process). It runs on the
	// deploy host; hemma enforces that. It first brings this checkout level with origin itself (pull --rebase,
	// push) and needs an upstream and a clean tree.
	{Words: []string{"deploy"}, Group: "Operate", Title: "Deploy (pull from origin + apply)", Timeout: 10 * time.Minute,
		Confirm: "Deploy? This pulls from origin, then makes the saved config live on every host."},

	{Words: []string{"verify"}, Group: "Diagnose", Title: "Verify live DNS/Caddy", ReadOnly: true, FlagsFirst: true, Timeout: 3 * time.Minute,
		Fields: []field{flag("all", "All services"), ref(pos("fqdn", "Service (optional)", false), "service-fqdn")}},
	{Words: []string{"measure"}, Group: "Diagnose", Title: "Measure request timing", ReadOnly: true, FlagsFirst: true, Timeout: 3 * time.Minute,
		Fields: []field{flag("compare", "A/B split-horizon vs public"), {Name: "runs", Label: "Timed runs", Kind: "int"}, {Name: "warmup", Label: "Warm-up runs", Kind: "int"},
			ref(pos("target", "Service", true), "service")}}, // a picker of your services (the CLI also takes a hostname or URL; the A/B compare is services-only anyway)
	{Words: []string{"doctor"}, Group: "Diagnose", Title: "Doctor (audit)", FlagsFirst: true, Timeout: 2 * time.Minute,
		Fields: []field{{Name: "fix", Label: "Apply fixes (writes files and .gitignore)", Kind: "bool"}}},

	// Not exposed on purpose: credential minting (create app/user), and the terminal-only
	// help, completion, version, `list` (the Services page IS the overview) and `defaults` listing (the Defaults tab shows the same data;
	// the version is shown in the page footer).
}

func init() {
	for i := range commands {
		c := &commands[i]
		c.Key = strings.Join(c.Words, " ")
		if c.Fields == nil {
			c.Fields = []field{}
		}
		if c.Timeout == 0 {
			c.Timeout = time.Minute
		}
	}
}

func lookup(key string) *command {
	for i := range commands {
		if commands[i].Key == key {
			return &commands[i]
		}
	}
	return nil
}

// build validates vals against the command's fields and returns the argv
// (after `hemma -C <repo>`).
func (c *command) build(vals map[string]any) ([]string, error) {
	var flags, positionals []string
	for _, f := range c.Fields {
		v, present := vals[f.Name]
		s := ""
		switch t := v.(type) {
		case nil:
		case string:
			s = strings.TrimSpace(t)
		case bool:
			s = strconv.FormatBool(t)
		case float64:
			s = strconv.Itoa(int(t))
		default:
			return nil, fmt.Errorf("%s: unsupported value", f.Name)
		}
		if f.Required && s == "" {
			return nil, fmt.Errorf("%s is required", f.Label)
		}
		if f.Options != nil && present && !contains(f.Options, s) && !(s == "" && f.Default != "") {
			return nil, fmt.Errorf("%s: %q is not an allowed value", f.Label, s)
		}
		// Typed values: shape-check here; "must exist in services.yaml" is checkState's job.
		if f.Ref != "" && !statefulRefs[f.Ref] && s != "" {
			items := []string{s}
			if f.Kind == "list" {
				items = strings.Split(s, ",")
			}
			for _, it := range items {
				if err := validateRef(f.Ref, strings.TrimSpace(it)); err != nil {
					return nil, fmt.Errorf("%s: %v", f.Label, err)
				}
			}
		}
		switch f.Kind {
		case "pos":
			if s == "" {
				continue
			}
			// '-' alone is the documented "clear" value of `defaults set`.
			if !posRe.MatchString(s) && !(s == "-" && c.Key == "defaults set") {
				return nil, fmt.Errorf("%s: invalid value", f.Label)
			}
			positionals = append(positionals, s)
		case "bool":
			if !present {
				continue
			}
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("%s: expected true/false", f.Label)
			}
			flags = append(flags, "--"+f.Name+"="+s)
		case "int":
			if s == "" {
				continue
			}
			if n, err := strconv.Atoi(s); err != nil || n < 0 || n > 1000 {
				return nil, fmt.Errorf("%s: expected a number 0-1000", f.Label)
			}
			flags = append(flags, "--"+f.Name+"="+s)
		default: // str, list, select
			if !present || (s == "" && !c.Update) {
				continue
			}
			if strings.ContainsAny(s, "\x00\r\n") {
				return nil, fmt.Errorf("%s: invalid value", f.Label)
			}
			flags = append(flags, "--"+f.Name+"="+s)
		}
	}
	argv := append([]string{}, c.Words...)
	if c.FlagsFirst {
		argv = append(append(argv, flags...), positionals...)
	} else {
		argv = append(append(argv, positionals...), flags...)
	}
	return argv, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
