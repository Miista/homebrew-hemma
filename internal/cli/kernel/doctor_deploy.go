package kernel

import "strings"

// The deploy-readiness check itself is diagDeployReadiness (doctor.go); these are the lookups it stands on.

// hostKnown reports whether ssh already trusts a host key for the given name
// (ssh-keygen -F searches the default known_hosts set, hashed entries
// included). Overridable in tests. An empty name is never known.
var hostKnown = func(env *Env, host string) bool {
	if host == "" {
		return false
	}
	return env.command("ssh-keygen", "-F", host).Run() == nil
}

// sshHostPart extracts the host from a verbatim ssh destination for the
// known_hosts lookup: `user@host` → `host`. An ssh_config alias passes
// through unchanged (ssh records the key under the name it connected with
// only when no HostName rewrite applies — a miss here is why hostKnown also
// tries the ip).
func sshHostPart(dest string) string {
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[i+1:]
	}
	return dest
}
