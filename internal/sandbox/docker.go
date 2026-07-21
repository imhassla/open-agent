package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Image is the sandbox image tag.
const Image = "open-agent-lab:latest"

// dockerfile is baked into the sandbox image at build time. The agent binary is
// staged next to it as ./open-agent (a linux build) by BuildImage; the egress
// firewall script is written for the --net https mode.
const dockerfile = `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates sudo git curl jq python3 python3-venv python3-pip \
      build-essential procps iproute2 iputils-ping iptables dnsutils \
      tree less nano vim unzip \
 && rm -rf /var/lib/apt/lists/*
COPY open-agent /usr/local/bin/open-agent
COPY oa-lockdown /usr/local/bin/oa-lockdown
RUN chmod 0755 /usr/local/bin/open-agent /usr/local/bin/oa-lockdown
WORKDIR /work
ENTRYPOINT ["/bin/bash"]
`

// lockdownScript installs an egress firewall INSIDE the container's own network
// namespace (needs NET_ADMIN, which cannot affect the host): DROP all outbound
// except loopback, established, DNS, and HTTPS(443). This blocks raw sockets,
// ssh, and non-standard-port exfil while still letting the agent reach the
// OpenRouter API and clone/pull over HTTPS. (Per-HOST allowlisting a CDN like
// OpenRouter by IP is unreliable, so this is deliberately HTTPS-scoped.)
const lockdownScript = `#!/bin/bash
set -e
iptables -F OUTPUT
iptables -P OUTPUT DROP
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT
iptables -A OUTPUT -p tcp --dport 443 -j ACCEPT
echo "egress locked: HTTPS(443)+DNS only"
`

// DockerAvailable reports whether a working docker CLI + daemon are present.
func DockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}

// ImageExists reports whether the sandbox image is already built.
func ImageExists() bool {
	return exec.Command("docker", "image", "inspect", Image).Run() == nil
}

// BuildImage builds the sandbox image, staging linuxBin (a linux/<arch> agent
// binary) into it. It writes the Dockerfile + a copy of the binary into a temp
// build context so the host tree is never the build context.
func BuildImage(ctx context.Context, linuxBin string, out io.Writer) error {
	data, err := os.ReadFile(linuxBin)
	if err != nil {
		return fmt.Errorf("staging agent binary: %w", err)
	}
	dir, err := os.MkdirTemp("", "oa-sbx-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "open-agent"), data, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "oa-lockdown"), []byte(lockdownScript), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", Image, dir)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// List returns the env names of existing sandbox containers (running or stopped).
func List() ([]string, error) {
	o, err := exec.Command("docker", "ps", "-a", "--filter", "name=oa-sbx-",
		"--format", "{{.Names}}").Output()
	if err != nil {
		return nil, err
	}
	var envs []string
	for _, ln := range strings.Split(strings.TrimSpace(string(o)), "\n") {
		if ln == "" {
			continue
		}
		if e := EnvFromContainer(ln); ValidEnvName(e) {
			envs = append(envs, e)
		}
	}
	return SortEnvs(envs), nil
}

// Remove force-removes an environment's container and its /work volume.
func Remove(env string) error {
	_ = exec.Command("docker", "rm", "-f", ContainerName(env)).Run()
	return exec.Command("docker", "volume", "rm", "-f", VolumeName(env)).Run()
}

// SeedRatings copies a host ratings.json into an environment's persistent /work
// volume at /work/.open-agent/ratings.json (HOME=/work inside), so a FRESH env
// starts with learned pass-rates instead of cold-starting at the free rung. It
// runs a short helper container that mounts ONLY that env's own volume — no host
// path is exposed to the sandbox. Idempotent-ish: overwrites the seed file.
func SeedRatings(ctx context.Context, env, ratingsPath string) error {
	data, err := os.ReadFile(ratingsPath)
	if err != nil {
		return fmt.Errorf("reading seed ratings: %w", err)
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"-v", VolumeName(env)+":/work", Image,
		"-lc", "mkdir -p /work/.open-agent && cat > /work/.open-agent/ratings.json")
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("seeding volume: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunSpec parameterizes a sandbox launch.
type RunSpec struct {
	Env         string   // environment name (isolated container + /work volume)
	Persist     bool     // keep /work in a named volume across sessions
	EnvFile     string   // --env-file with the OpenRouter key (never baked into the image)
	Limits      Limits   // resource ceilings
	Network     Network  // egress posture (default NetFull)
	Interactive bool     // allocate a TTY (shell) vs one-shot exec
	Cmd         []string // command to run inside (empty ⇒ interactive bash)
}

// RunArgs assembles the full `docker run` argv (excluding the leading "docker")
// for a spec — the single place the hardened flags, env isolation, and image
// come together. Exposed (not just Run) so it is unit-testable without Docker.
func (s RunSpec) RunArgs() []string {
	vol := ""
	if s.Persist {
		vol = VolumeName(s.Env)
	}
	net := s.Network
	if net == "" {
		net = NetFull
	}
	argv := []string{"run", "--rm"}
	if s.Interactive {
		argv = append(argv, "-it")
	}
	argv = append(argv, HardenedArgs(ContainerName(s.Env), "oa-"+s.Env, vol, s.Limits, net)...)
	if s.EnvFile != "" {
		argv = append(argv, "--env-file", s.EnvFile)
	}
	argv = append(argv, Image)
	// In HTTPS-locked mode the entrypoint installs the egress firewall (needs the
	// added NET_ADMIN cap) before handing control to the requested command.
	inner := strings.Join(s.Cmd, " ")
	if net == NetHTTPS {
		lock := "oa-lockdown >&2 || true"
		if inner == "" {
			inner = "exec bash"
		}
		argv = append(argv, "-lc", lock+"; "+inner)
	} else if len(s.Cmd) > 0 {
		argv = append(argv, "-lc", inner)
	}
	return argv
}

// Run launches the container, wiring the caller's stdio through (so an
// interactive shell or a streamed task run behaves normally).
func (s RunSpec) Run(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", s.RunArgs()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
