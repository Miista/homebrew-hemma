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
	Kind     string   `json:"kind"` // pos | str | bool | int | select
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	Help     string   `json:"help,omitempty"`
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
	authModes = []string{"", "none", "forward", "oidc", "external"}
	levels    = []string{"", "bypass", "one_factor", "two_factor", "deny"}
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	posRe     = regexp.MustCompile(`^[^\s\x00-\x1f-][^\s\x00-\x1f]*$`)
)

func pos(name, label string, req bool) field {
	return field{Name: name, Label: label, Kind: "pos", Required: req}
}
func str(name, label string) field  { return field{Name: name, Label: label, Kind: "str"} }
func flag(name, label string) field { return field{Name: name, Label: label, Kind: "bool"} }

func serviceFields(add bool) []field {
	f := []field{
		pos("name", "Name", true),
		{Name: "fqdn", Label: "FQDN", Kind: "str", Required: add},
		{Name: "host", Label: "Host", Kind: "str", Required: add},
		{Name: "backend", Label: "Backend (name:port)", Kind: "str", Required: add},
		{Name: "auth-mode", Label: "Auth mode", Kind: "select", Options: authModes},
		str("auth-groups", "Auth groups (comma-separated)"),
		str("bypass-paths", "Bypass paths (comma-separated, forward mode)"),
		{Name: "auth-policy-local", Label: "Policy: local horizon", Kind: "select", Options: levels},
		{Name: "auth-policy-public", Label: "Policy: public horizon", Kind: "select", Options: levels},
		flag("tunnel", "Reachable from the internet (tunnel)"),
	}
	return f
}

var commands = []command{
	{Words: []string{"add", "service"}, Group: "Services", Title: "Add service", Fields: serviceFields(true), Timeout: 2 * time.Minute},
	{Words: []string{"update", "service"}, Group: "Services", Title: "Update service", Fields: serviceFields(false), Update: true, Timeout: 2 * time.Minute},
	{Words: []string{"remove", "service"}, Group: "Services", Title: "Remove service", Fields: []field{pos("name", "Name", true)}, Confirm: "Remove this service and its generated files?"},
	{Words: []string{"disable", "service"}, Group: "Services", Title: "Disable service", Fields: []field{pos("name", "Name", true)}},
	{Words: []string{"enable", "service"}, Group: "Services", Title: "Enable service", Fields: []field{pos("name", "Name", true)}},
	{Words: []string{"rename", "service"}, Group: "Services", Title: "Rename service", Fields: []field{pos("old", "Current name", true), pos("new", "New name", true)}},

	{Words: []string{"add", "host"}, Group: "Hosts", Title: "Add host", Fields: []field{pos("name", "Name", true), pos("ip", "IP", true), str("trusted-proxies", "Extra trusted proxies (comma-separated)")}},
	{Words: []string{"update", "host"}, Group: "Hosts", Title: "Update host", Update: true, Fields: []field{pos("name", "Name", true), str("ip", "IP"), str("trusted-proxies", "Extra trusted proxies (comma-separated)")}},
	{Words: []string{"remove", "host"}, Group: "Hosts", Title: "Remove host", Fields: []field{pos("name", "Name", true)}, Confirm: "Remove this host?"},

	{Words: []string{"add", "domain"}, Group: "Domains", Title: "Add domain", Fields: []field{pos("name", "Domain", true)}},
	{Words: []string{"remove", "domain"}, Group: "Domains", Title: "Remove domain", Fields: []field{pos("name", "Domain", true)}, Confirm: "Remove this domain?"},

	{Words: []string{"defaults"}, Group: "Defaults", Title: "Show defaults", ReadOnly: true},
	{Words: []string{"defaults", "set"}, Group: "Defaults", Title: "Set default", Fields: []field{
		{Name: "key", Label: "Setting", Kind: "pos", Required: true, Options: []string{"dns-host", "auth-snippet", "auth-service", "local-networks", "tunnel-dir"}},
		{Name: "value", Label: "Value ('-' clears)", Kind: "pos", Required: true}}},

	{Words: []string{"create", "app", "oidc"}, Group: "Credentials", Title: "Create OIDC app credentials", Fields: []field{pos("app_name", "App name", true), pos("callback_path", "Callback path", false)},
		Help: "Prints a client id, secret and config snippet. The secret is shown once and not stored."},

	{Words: []string{"apply"}, Group: "Operate", Title: "Apply to this host", Timeout: 5 * time.Minute, Fields: nil,
		Confirm: "Apply now? This restarts pihole, reloads caddy and restarts the auth provider on this host."},

	{Words: []string{"list"}, Group: "Diagnose", Title: "List overview", ReadOnly: true, FlagsFirst: true,
		Fields: []field{flag("all", "All hosts"), flag("policy", "Show policy columns")}},
	{Words: []string{"verify"}, Group: "Diagnose", Title: "Verify live DNS/Caddy", ReadOnly: true, FlagsFirst: true, Timeout: 3 * time.Minute,
		Fields: []field{flag("all", "All services"), pos("fqdn", "FQDN (optional)", false)}},
	{Words: []string{"measure"}, Group: "Diagnose", Title: "Measure request timing", ReadOnly: true, FlagsFirst: true, Timeout: 3 * time.Minute,
		Fields: []field{flag("compare", "A/B split-horizon vs public"), {Name: "runs", Label: "Timed runs", Kind: "int"}, {Name: "warmup", Label: "Warm-up runs", Kind: "int"}, pos("target", "Service, hostname or URL", true)}},
	{Words: []string{"doctor"}, Group: "Diagnose", Title: "Doctor (audit)", FlagsFirst: true, Timeout: 2 * time.Minute,
		Fields: []field{{Name: "fix", Label: "Apply fixes (writes files and .gitignore)", Kind: "bool"}}},

	{Words: []string{"version"}, Group: "About", Title: "Version", ReadOnly: true},
	{Words: []string{"help"}, Group: "About", Title: "Help", ReadOnly: true, Fields: []field{pos("command", "Command (optional)", false)}},
	{Words: []string{"completion"}, Group: "About", Title: "Shell completion script", ReadOnly: true,
		Fields: []field{{Name: "shell", Label: "Shell", Kind: "pos", Required: true, Options: []string{"bash", "zsh"}}}},
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
		if f.Options != nil && present && !contains(f.Options, s) {
			return nil, fmt.Errorf("%s: %q is not an allowed value", f.Label, s)
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
		default: // str, select
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
