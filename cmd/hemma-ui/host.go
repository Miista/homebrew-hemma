package main

import (
	"errors"
	"path/filepath"

	"hemma/internal/config"
)

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

var errNoDeployHost = errors.New("Set the deploy host first (Infrastructure → Settings → Deploy host). This page deploys from that host and does not guess which one it is.")

// needDeployHost is nil when there is a deploy host (a declared one), and otherwise says what to do. Whatever makes the saved
// config live (deploy, apply) asks first: without a deploy host this page does not know where it is running. The kernel only
// refuses a deploy from the WRONG host; with none set it would run anywhere, so this check is the UI's own.
func (s *server) needDeployHost() error {
	cfg, err := config.Load(filepath.Join(s.repo, "services.yaml"))
	if err != nil {
		return err
	}
	if deployHostSelf(cfg) == "" {
		return errNoDeployHost
	}
	return nil
}

// makesLive is the allowlisted commands that put the saved config on a host (and so need to know which one this is).
func makesLive(key string) bool { return key == "deploy" || key == "apply" }
