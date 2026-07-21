// Package sandbox runs open-agent inside a hardened, throwaway Docker container
// so an agent can be given ROOT for real automation while the host stays
// untouchable. It is the single source of truth for the container's security
// posture and for multi-environment (named, isolated session) management.
//
// Safety model: container namespaces already isolate the agent; the flag set in
// HardenedArgs refuses every way that isolation is normally broken (no
// --privileged, no host bind mounts, no docker.sock, no host namespaces, default
// seccomp+AppArmor kept, escape-prone capabilities dropped) and caps resource
// abuse (pids/memory/cpu). Root INSIDE, harmless to the host OUTSIDE.
package sandbox

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits are the resource ceilings applied to every sandbox container.
type Limits struct {
	Memory string // e.g. "3g"
	CPUs   string // e.g. "2"
	Pids   int    // max processes (fork-bomb ceiling)
}

// DefaultLimits are conservative ceilings suitable for a coding/automation box.
func DefaultLimits() Limits { return Limits{Memory: "3g", CPUs: "2", Pids: 512} }

// escape-prone capabilities dropped on top of Docker's already-reduced default
// set: each is a known container-escape or host-tampering primitive that no
// coding/automation workload needs.
var droppedCaps = []string{
	"SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_RAWIO",
	"SYS_BOOT", "SYS_TIME", "MKNOD", "NET_RAW",
}

// Network selects the container's egress posture.
type Network string

const (
	NetFull  Network = "full"  // default bridge — apt, git clone, OpenRouter, everything
	NetNone  Network = "none"  // fully offline — the strongest isolation for untrusted code
	NetHTTPS Network = "https" // outbound HTTPS (443) + DNS only — blocks raw/ssh/non-standard exfil
)

// KnownNetwork validates a --net value.
func KnownNetwork(n Network) bool { return n == NetFull || n == NetNone || n == NetHTTPS }

// HardenedArgs returns the `docker run` arguments (everything AFTER `run`, up to
// but not including the image) that enforce the sandbox's security posture and
// resource limits. It deliberately does NOT include -v/--privileged/host-
// namespace flags — their ABSENCE is the guarantee. name and hostname identify
// the container; workVol (a named volume) persists /work across sessions when
// non-empty, else /work is ephemeral tmpfs. net sets the egress posture.
func HardenedArgs(name, hostname, workVol string, lim Limits, net Network) []string {
	args := []string{
		"--name", name,
		"--hostname", hostname,
		// no-new-privileges is intentionally OFF: the agent runs AS root (already
		// max privilege inside), and ON would break sudo/setuid for no host benefit.
		"--security-opt", "no-new-privileges=false",
	}
	for _, c := range droppedCaps {
		args = append(args, "--cap-drop", c)
	}
	switch net {
	case NetNone:
		args = append(args, "--network", "none")
	case NetHTTPS:
		// The lockdown entrypoint needs NET_ADMIN to install the egress firewall
		// INSIDE its own (isolated) network namespace — this cannot touch the host.
		args = append(args, "--cap-add", "NET_ADMIN")
	}
	args = append(args,
		"--pids-limit", fmt.Sprint(lim.Pids),
		"--memory", lim.Memory,
		"--memory-swap", lim.Memory, // == memory ⇒ no swap headroom
		"--cpus", lim.CPUs,
		"--tmpfs", "/tmp:rw,exec,size=512m",
	)
	if workVol != "" {
		// Persist /work AND the agent's state dir: HOME=/work means ratings,
		// telemetry and memory accumulate in the volume, so a reused env warms up
		// (no cold-start free-rung penalty on the second session).
		args = append(args, "-v", workVol+":/work", "-e", "HOME=/work")
	} else {
		args = append(args, "--tmpfs", "/work:rw,exec,size=1g")
	}
	return args
}

// envNameRe constrains environment names to a safe, docker-friendly charset so
// they compose into container/volume names without injection or escaping issues.
var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)

// ValidEnvName reports whether name is a usable environment identifier.
func ValidEnvName(name string) bool { return envNameRe.MatchString(name) }

// ContainerName / VolumeName derive the docker object names for an environment.
// Distinct environments get distinct containers AND distinct /work volumes, so
// their sessions are fully isolated from each other (and from the host).
func ContainerName(env string) string { return "oa-sbx-" + env }
func VolumeName(env string) string    { return "oa-sbx-" + env + "-work" }

// EnvFromContainer inverts ContainerName (for `sandbox ls`); "" if not ours.
func EnvFromContainer(container string) string {
	return strings.TrimPrefix(container, "oa-sbx-")
}

// SortEnvs returns env names in stable order (for deterministic listing).
func SortEnvs(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
