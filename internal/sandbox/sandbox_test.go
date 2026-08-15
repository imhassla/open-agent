package sandbox

import (
	"strings"
	"testing"
)

// The hardened flag set must ENFORCE the security posture and, just as
// importantly, must never contain a host-exposing flag — the absence is the
// guarantee. This test pins both directions.
func TestHardenedArgsPosture(t *testing.T) {
	joined := strings.Join(HardenedArgs("c", "h", "", DefaultLimits(), NetFull), " ")

	mustHave := []string{
		"--security-opt no-new-privileges=false",
		"--cap-drop SYS_ADMIN", "--cap-drop SYS_MODULE", "--cap-drop SYS_PTRACE",
		"--cap-drop NET_RAW", "--pids-limit 512", "--memory 3g", "--cpus 2",
	}
	for _, s := range mustHave {
		if !strings.Contains(joined, s) {
			t.Errorf("hardened args missing %q; got: %s", s, joined)
		}
	}
	// Host-exposing flags must NEVER appear.
	mustNotHave := []string{
		"--privileged", "--pid=host", "--pid host", "--network=host",
		"--network host", "--ipc=host", "docker.sock", "seccomp=unconfined",
		"apparmor=unconfined", "cap-add ALL",
	}
	for _, s := range mustNotHave {
		if strings.Contains(joined, s) {
			t.Errorf("hardened args must NOT contain %q; got: %s", s, joined)
		}
	}
	// memory-swap == memory ⇒ no swap headroom.
	if !strings.Contains(joined, "--memory-swap 3g") {
		t.Error("swap must be pinned to the memory limit")
	}
}

// Persistent envs get a named /work volume; ephemeral envs get a tmpfs and no
// bind at all.
func TestWorkVolumeVsTmpfs(t *testing.T) {
	persist := strings.Join(HardenedArgs("c", "h", VolumeName("dev"), DefaultLimits(), NetFull), " ")
	if !strings.Contains(persist, "-v oa-sbx-dev-work:/work") {
		t.Errorf("persistent env must bind its named volume, got: %s", persist)
	}
	ephemeral := strings.Join(HardenedArgs("c", "h", "", DefaultLimits(), NetFull), " ")
	if strings.Contains(ephemeral, "-v ") {
		t.Errorf("ephemeral env must not bind any volume, got: %s", ephemeral)
	}
	if !strings.Contains(ephemeral, "--tmpfs /work") {
		t.Errorf("ephemeral env must tmpfs /work, got: %s", ephemeral)
	}
}

func TestEnvNaming(t *testing.T) {
	for _, ok := range []string{"dev", "test-1", "a", "ci_run", "x2"} {
		if !ValidEnvName(ok) {
			t.Errorf("%q should be a valid env name", ok)
		}
	}
	for _, bad := range []string{"", "-x", "UP", "a b", "a;rm", "../e", strings.Repeat("x", 40)} {
		if ValidEnvName(bad) {
			t.Errorf("%q must be rejected (injection/format safety)", bad)
		}
	}
	if ContainerName("dev") != "oa-sbx-dev" || VolumeName("dev") != "oa-sbx-dev-work" {
		t.Error("name derivation drift")
	}
	if EnvFromContainer("oa-sbx-ci") != "ci" {
		t.Error("EnvFromContainer must invert ContainerName")
	}
}

// Distinct environments must map to distinct containers AND distinct /work
// volumes — the isolation-between-sessions invariant.
func TestEnvIsolation(t *testing.T) {
	a := RunSpec{Env: "alpha", Persist: true, Limits: DefaultLimits()}.RunArgs()
	b := RunSpec{Env: "beta", Persist: true, Limits: DefaultLimits()}.RunArgs()
	ja, jb := strings.Join(a, " "), strings.Join(b, " ")
	if !strings.Contains(ja, "oa-sbx-alpha") || !strings.Contains(ja, "oa-sbx-alpha-work") {
		t.Error("alpha env not wired to its own container/volume")
	}
	if strings.Contains(ja, "beta") || strings.Contains(jb, "alpha") {
		t.Error("environments must not reference each other")
	}
	// The image is always the last arg before any inner command.
	if !strings.Contains(ja, Image) {
		t.Error("run args must target the sandbox image")
	}
}

func TestRunArgsCmdVsInteractive(t *testing.T) {
	oneShot := RunSpec{Env: "e", Cmd: []string{"open-agent", "code", "x"}, Limits: DefaultLimits()}.RunArgs()
	js := strings.Join(oneShot, " ")
	if strings.Contains(js, "-it ") {
		t.Error("a one-shot command run must not allocate a TTY")
	}
	if !strings.HasSuffix(js, `-lc open-agent code x`) {
		t.Errorf("inner command must be passed via -lc, got: %s", js)
	}
	shell := RunSpec{Env: "e", Interactive: true, Limits: DefaultLimits()}.RunArgs()
	if !strings.Contains(strings.Join(shell, " "), " -it ") {
		t.Error("an interactive shell must allocate a TTY")
	}
}

// Network modes: none → --network none; https → NET_ADMIN added for the egress
// firewall + the lockdown entrypoint prefixed; full → neither.
func TestNetworkModes(t *testing.T) {
	none := strings.Join(RunSpec{Env: "e", Network: NetNone, Limits: DefaultLimits()}.RunArgs(), " ")
	if !strings.Contains(none, "--network none") {
		t.Errorf("none mode must go offline, got: %s", none)
	}

	https := RunSpec{Env: "e", Network: NetHTTPS, Cmd: []string{"open-agent", "code", "x"}, Limits: DefaultLimits()}.RunArgs()
	jh := strings.Join(https, " ")
	if !strings.Contains(jh, "--cap-add NET_ADMIN") {
		t.Errorf("https mode needs NET_ADMIN for the in-namespace firewall, got: %s", jh)
	}
	if !strings.Contains(jh, "oa-lockdown") {
		t.Errorf("https mode must install the egress firewall before the command, got: %s", jh)
	}
	// Fail-closed: lockdown must contain exit 97 and must NOT contain || true
	if !strings.Contains(jh, "exit 97") {
		t.Errorf("https mode must fail-closed with exit 97 on lockdown failure, got: %s", jh)
	}
	if strings.Contains(jh, "|| true") {
		t.Errorf("https mode must NOT use fail-open '|| true', got: %s", jh)
	}
	// The firewall must run BEFORE the agent command.
	if strings.Index(jh, "oa-lockdown") > strings.Index(jh, "open-agent code x") {
		t.Error("lockdown must precede the inner command")
	}

	full := strings.Join(RunSpec{Env: "e", Network: NetFull, Limits: DefaultLimits()}.RunArgs(), " ")
	if strings.Contains(full, "--network none") || strings.Contains(full, "NET_ADMIN") {
		t.Errorf("full mode must not restrict the network, got: %s", full)
	}
	if !KnownNetwork(NetFull) || !KnownNetwork(NetNone) || !KnownNetwork(NetHTTPS) || KnownNetwork("bogus") {
		t.Error("KnownNetwork validation drift")
	}
}

// A persistent env routes HOME to /work so the agent's ratings/telemetry/memory
// accumulate in the volume (warm-up across sessions); ephemeral does not.
func TestPersistentHomeState(t *testing.T) {
	persist := strings.Join(RunSpec{Env: "e", Persist: true, Limits: DefaultLimits()}.RunArgs(), " ")
	if !strings.Contains(persist, "-e HOME=/work") {
		t.Errorf("persistent env must put agent state on the volume via HOME=/work, got: %s", persist)
	}
	ephemeral := strings.Join(RunSpec{Env: "e", Persist: false, Limits: DefaultLimits()}.RunArgs(), " ")
	if strings.Contains(ephemeral, "HOME=/work") {
		t.Errorf("ephemeral env must not persist state, got: %s", ephemeral)
	}
}
