package kernel

import (
	"os"
	"path/filepath"
)

// Run is a test stand-in for the CLI front end: it handles the global -C flag
// and routes to the kernel, exactly as cli.Run does for kernel commands. (The
// real cli.Run cannot be imported here — cli imports kernel — and its own
// behaviour is pinned by the snapshot tests in package cli.)
func Run(args []string) int {
	repoRoot := "~/docker"
	if home, err := os.UserHomeDir(); err == nil {
		repoRoot = filepath.Join(home, "docker")
	}
	if len(args) >= 2 && (args[0] == "-C" || args[0] == "--chdir") {
		repoRoot = args[1]
		args = args[2:]
	}
	if len(args) < 1 {
		return 2
	}
	code, ok := Route(stdEnv(), repoRoot, filepath.Join(repoRoot, configName), args[0], args[1:])
	if !ok {
		return 2
	}
	return code
}
