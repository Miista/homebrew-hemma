package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Read-only verification that the generated access-control artifact (§4.6) is
// actually wired into Authelia. hemma writes the artifact but never touches
// docker-compose.yml or configuration.yml (same non-goal as §12), so all it
// can do is parse the compose file from the checkout and check that the
// Authelia service's X_AUTHELIA_CONFIG lists the artifact — and, because
// doctor's advisories must carry the concrete fix when hemma can't --fix, the
// warnings include the exact env value to paste in.
//
// Secrets: compose files carry OIDC/session/storage secrets. Warnings quote
// ONLY the X_AUTHELIA_CONFIG value (a list of config paths), never any other
// environment entry or compose content.

const (
	// composeFileName is the conventional compose filename in a host's repo
	// directory (<hostDir>/docker-compose.yml).
	composeFileName = "docker-compose.yml"
	// autheliaConfigEnv is the Authelia environment variable naming the
	// comma-separated list of config files to load.
	autheliaConfigEnv = "X_AUTHELIA_CONFIG"
	// autheliaContainerConfigDir is where the config dir is mounted inside
	// the Authelia container by convention (configuration.yml lives there).
	autheliaContainerConfigDir = "/config"
)

// composeDoc is the sliver of docker-compose.yml this check reads: service
// names, container_name overrides, and environment. Environment stays a
// yaml.Node because compose allows both the map form (KEY: value) and the
// list form (- KEY=value).
type composeDoc struct {
	Services map[string]struct {
		ContainerName string    `yaml:"container_name"`
		Environment   yaml.Node `yaml:"environment"`
	} `yaml:"services"`
}

// autheliaTopLevelDoc detects a hand-written top-level access_control section
// in configuration.yml (only the key's presence; the content is never read
// into warnings).
type autheliaTopLevelDoc struct {
	AccessControl yaml.Node `yaml:"access_control"`
}

// ValidateWiring checks, read-only and docker-free, that the generated
// access-control artifact is loaded by the Authelia container declared in
// <hostDir>/docker-compose.yml (the service whose name — or container_name —
// matches container):
//
//   - X_AUTHELIA_CONFIG absent, or present without an entry whose basename is
//     the artifact filename → the artifact is generated but silently unused;
//     the warning carries the exact env value to set (existing entries
//     preserved, /config/<artifact> appended).
//   - configuration.yml also defines a top-level access_control section while
//     the artifact renders one → Authelia does not merge rule lists across
//     config files, one silently wins; the warning says to remove the
//     hand-written section once the generated file is wired in.
//
// Gated on the artifact actually being emitted for these services (nil
// otherwise — nothing to wire). A missing/unparseable compose file, or no
// matching service in it, degrades to a single soft could-not-verify advisory.
func (a authelia) ValidateWiring(hostDir, container string, services []Service) []string {
	relPath, content, ok := a.AccessControl(services)
	if !ok {
		return nil // artifact not part of the plan — nothing to wire.
	}
	artifact := filepath.Base(relPath)
	composePath := filepath.Join(hostDir, composeFileName)

	envValue, envFound, err := composeEnvValue(composePath, container, autheliaConfigEnv)
	if err != nil {
		return []string{fmt.Sprintf("could not verify that Authelia loads the generated %s: %v", artifact, err)}
	}

	var w []string
	wired := false
	if envFound {
		for _, entry := range strings.Split(envValue, ",") {
			if filepath.Base(strings.TrimSpace(entry)) == artifact {
				wired = true
				break
			}
		}
	}
	if !wired {
		// Full recipe, not just a diagnosis: hemma never writes the compose
		// file, so the exact value to paste in must appear here.
		want := autheliaContainerConfigDir + "/" + filepath.Base(autheliaConfigPath) + "," + autheliaContainerConfigDir + "/" + artifact
		if envFound {
			want = envValue + "," + autheliaContainerConfigDir + "/" + artifact
		}
		detail := fmt.Sprintf("%s is not set on it", autheliaConfigEnv)
		if envFound {
			detail = fmt.Sprintf("its %s=%q does not list the file", autheliaConfigEnv, envValue)
		}
		w = append(w, fmt.Sprintf(
			"%s is generated but Authelia does not load it — %s. Set on the %s service in %s:\n    %s: '%s'\n  then restart Authelia (hemma apply).",
			artifact, detail, container, composePath, autheliaConfigEnv, want))
	}

	// Duplicate access_control: only meaningful when the artifact itself
	// renders an access_control section (oidc-only artifacts don't).
	if strings.Contains(content, "\naccess_control:\n") {
		cfgPath := filepath.Join(hostDir, autheliaConfigPath)
		if hasTopLevelAccessControl(cfgPath) {
			w = append(w, fmt.Sprintf(
				"%s defines a top-level access_control section and the generated %s renders one too — Authelia does not merge access_control rule lists across config files, one silently wins. Once the generated file is wired in, remove the hand-written access_control section from %s and restart Authelia (hemma apply).",
				cfgPath, artifact, filepath.Base(cfgPath)))
		}
	}
	return w
}

// composeEnvValue parses the compose file at composePath, locates the service
// whose name or container_name equals container, and returns the value of env
// key on it. found=false means the service exists but the variable is unset;
// an error means the wiring can't be verified at all (missing/unparseable
// compose, or no matching service).
func composeEnvValue(composePath, container, key string) (value string, found bool, err error) {
	data, err := os.ReadFile(composePath)
	if err != nil {
		return "", false, err
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", composePath, err)
	}
	for name, svc := range doc.Services {
		if name != container && svc.ContainerName != container {
			continue
		}
		v, ok := envLookup(svc.Environment, key)
		return v, ok, nil
	}
	return "", false, fmt.Errorf("no service %q in %s", container, composePath)
}

// envLookup extracts key from a compose environment node, accepting both the
// map form (KEY: value) and the list form (- KEY=value).
func envLookup(env yaml.Node, key string) (string, bool) {
	switch env.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(env.Content); i += 2 {
			if env.Content[i].Value == key {
				return env.Content[i+1].Value, true
			}
		}
	case yaml.SequenceNode:
		for _, item := range env.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok && k == key {
				return v, true
			}
		}
	}
	return "", false
}

// hasTopLevelAccessControl reports whether configuration.yml declares a
// top-level access_control key. Read-only, key presence only; a missing or
// unparseable config is treated as "no" (its problems are surfaced by the
// other validations, not here).
func hasTopLevelAccessControl(cfgPath string) bool {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false
	}
	var doc autheliaTopLevelDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false
	}
	return !doc.AccessControl.IsZero()
}
