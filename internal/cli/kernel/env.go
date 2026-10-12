package kernel

import (
	"io"
	"os"

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
}

// interactive reports whether a command may prompt on the process's terminal.
func (e *Env) interactive() bool {
	return !e.NoTerminal && term.IsTerminal(int(os.Stdin.Fd()))
}

// StdEnv is the process's own stdout/stderr, read at call time (so a caller
// that swaps os.Stdout, as tests do, is honoured).
func StdEnv() *Env { return stdEnv() }

func stdEnv() *Env { return &Env{Out: os.Stdout, Err: os.Stderr} }
