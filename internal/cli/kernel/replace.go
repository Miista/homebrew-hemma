package kernel

import (
	"fmt"

	"hemma/internal/config"
	syncpkg "hemma/internal/sync"
)

// ReplaceConfig replaces services.yaml wholesale with text: the recovery path for a file that cannot be
// loaded, which no add/update/remove command can touch (they all start by loading it). It is not a CLI
// command — someone at a shell has an editor — but a front end without one needs it, and it must still go
// through the kernel: the single writer of services.yaml and of everything generated from it.
//
// Text that does not parse is refused and nothing is written, so the repo is never left in a state
// hemma cannot read. Text that parses is written exactly as given (comments and ordering kept until the
// next command normalises the file) and then synced like every other mutation.
func ReplaceConfig(env *Env, repoRoot, cfgPath, text string) int {
	cfg, err := config.Parse([]byte(text), cfgPath)
	if err != nil {
		errf(env, "%v", err)
		return 2
	}
	if err := config.AtomicWrite(cfgPath, []byte(text)); err != nil {
		errf(env, "Could not write services.yaml: %v", err)
		return 1
	}
	fmt.Fprintln(env.Out, tick+" Saved services.yaml")
	return runSync(env, repoRoot, cfg, syncpkg.Complete)
}
