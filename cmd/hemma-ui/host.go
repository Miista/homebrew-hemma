package main

import "hemma/internal/config"

// The UI always runs on the deploy host, so it does not ask the network interfaces which host it is (that is why it once had to
// sit on the host network: a bridge IP matches no host). It is whichever host defaults.deploy_host names. Only this process uses
// it: the kernel and the CLI keep matching their own interfaces.
//
// Unset, or naming a host that does not exist, it is "" — the kernel's own "matches no declared host".
func deployHostSelf(cfg *config.Config) string {
	dh := cfg.Defaults.DeployHost
	if _, ok := cfg.Hosts[dh]; !ok {
		return ""
	}
	return dh
}
