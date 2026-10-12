package kernel

import (
	"context"
	"io"
	"os"
	"os/exec"

	"golang.org/x/term"
)

// Env is where a command's output goes. The CLI passes the process's stdout
// and stderr; another front end (the web UI) passes buffers to capture what a
// command prints. Commands never touch os.Stdout/os.Stderr directly, so
// concurrent runs with separate Envs do not interleave.
type Env struct {
	Out io.Writer // normal output (stdout)
	Err io.Writer // errors, hints, warnings (stderr)

	// NoTerminal forces commands that would open an interactive terminal form
	// (add/update with no flags, create user) to refuse instead, whatever the
	// process's stdin is. A non-terminal front end sets it.
	NoTerminal bool

	// Ctx, when set, bounds every external command the kernel starts
	// (docker, caddy, dig, ssh, git, curl, bash): cancelling it kills them.
	// nil means no bound.
	Ctx context.Context
}

// DisableColor makes the status glyphs plain text for the rest of the process.
// Glyph colour is decided once at start-up from the process's own terminal; a
// front end that captures output (the web UI) calls this so a terminal it
// happens to run in never leaks escape codes into captured text.
func DisableColor() {
	tick, cross, warn = "✓", "✗", "⚠"
	boldOn, boldOff, dimOn, dimOff = "", "", "", ""
}

// command builds an external command that honours the Env's context.
func (e *Env) command(name string, args ...string) *exec.Cmd {
	ctx := e.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, name, args...)
}

// interactive reports whether a command may prompt on the process's terminal.
func (e *Env) interactive() bool {
	return !e.NoTerminal && term.IsTerminal(int(os.Stdin.Fd()))
}

// StdEnv is the process's own stdout/stderr, read at call time (so a caller
// that swaps os.Stdout, as tests do, is honoured).
func StdEnv() *Env { return stdEnv() }

func stdEnv() *Env { return &Env{Out: os.Stdout, Err: os.Stderr} }
