package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/imhassla/open-agent/internal/rating"
	"github.com/imhassla/open-agent/internal/sandbox"
)

// runSandbox dispatches `open-agent sandbox <sub> [--env NAME] [args...]` — a
// first-class hardened-container mode: give the agent ROOT in a throwaway Ubuntu
// box, isolated from the host, with named multi-environment sessions.
//
//	sandbox build                 build/refresh the sandbox image
//	sandbox shell   [--env NAME]  open a root shell in the box (persistent /work)
//	sandbox run     [--env NAME] "<task>"   run one agent code task inside
//	sandbox exec    [--env NAME] -- <cmd…>  run a raw shell command inside
//	sandbox ls                    list environments
//	sandbox rm      --env NAME     destroy an environment (container + /work)
func runSandbox(args []string, cfgEnvFile string) {
	env := "default"
	persist := true
	net := sandbox.NetFull
	seed := true // auto-seed a fresh persistent env from host ratings
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--env":
			if i+1 < len(args) {
				i++
				env = args[i]
			}
		case "--net":
			if i+1 < len(args) {
				i++
				net = sandbox.Network(args[i])
			}
		case "--ephemeral":
			persist = false
		case "--no-seed":
			seed = false
		case "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		default:
			rest = append(rest, args[i])
		}
	}
	if !sandbox.KnownNetwork(net) {
		fmt.Fprintf(os.Stderr, "invalid --net %q (full|none|https)\n", net)
		os.Exit(2)
	}
	var sub string
	if len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	if !sandbox.ValidEnvName(env) {
		fmt.Fprintf(os.Stderr, "invalid --env %q (use [a-z0-9][a-z0-9_-]*)\n", env)
		os.Exit(2)
	}
	if !sandbox.DockerAvailable() {
		fmt.Fprintln(os.Stderr, "sandbox needs a running Docker daemon (docker info failed)")
		os.Exit(1)
	}

	switch sub {
	case "build":
		sandboxBuild()
	case "ls":
		envs, err := sandbox.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "list error:", err)
			os.Exit(1)
		}
		if len(envs) == 0 {
			fmt.Println("(no sandbox environments)")
			return
		}
		for _, e := range envs {
			fmt.Println(e)
		}
	case "rm":
		if err := sandbox.Remove(env); err != nil {
			fmt.Fprintln(os.Stderr, "remove error:", err)
			os.Exit(1)
		}
		fmt.Printf("removed environment %q\n", env)
	case "seed":
		src := rating.DefaultPath()
		if len(rest) > 0 {
			src = rest[0]
		}
		if err := sandbox.SeedRatings(context.Background(), env, src); err != nil {
			fmt.Fprintln(os.Stderr, "seed error:", err)
			os.Exit(1)
		}
		fmt.Printf("seeded %q with ratings from %s\n", env, src)
	case "shell":
		maybeSeed(env, persist, seed)
		sandboxLaunch(sandbox.RunSpec{Env: env, Persist: persist, EnvFile: cfgEnvFile,
			Limits: sandbox.DefaultLimits(), Network: net, Interactive: true})
	case "run":
		task := strings.TrimSpace(strings.Join(rest, " "))
		if task == "" {
			fmt.Fprintln(os.Stderr, `usage: open-agent sandbox run [--env NAME] "<task>"`)
			os.Exit(2)
		}
		maybeSeed(env, persist, seed)
		// Build the inner agent invocation. cd /work so edits land on the
		// persistent volume; --json keeps the result machine-readable.
		inner := fmt.Sprintf("cd /work && open-agent code --json --max-cost 0.15 %s", shellQuote(task))
		sandboxLaunch(sandbox.RunSpec{Env: env, Persist: persist, EnvFile: cfgEnvFile,
			Limits: sandbox.DefaultLimits(), Network: net, Cmd: []string{inner}})
	case "exec":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "usage: open-agent sandbox exec [--env NAME] -- <command…>")
			os.Exit(2)
		}
		sandboxLaunch(sandbox.RunSpec{Env: env, Persist: persist, EnvFile: cfgEnvFile,
			Limits: sandbox.DefaultLimits(), Network: net, Cmd: rest})
	default:
		fmt.Fprintln(os.Stderr, sandboxUsage)
		os.Exit(2)
	}
}

// maybeSeed auto-seeds a persistent env's volume from host ratings the first
// time it is used (best-effort, silent on failure) — so a fresh env skips the
// cold-start free-rung penalty. Skipped for ephemeral envs and when the volume
// already exists (already warmed / previously seeded).
func maybeSeed(env string, persist, seed bool) {
	if !persist || !seed {
		return
	}
	if exec.Command("docker", "volume", "inspect", sandbox.VolumeName(env)).Run() == nil {
		return // volume exists → already warmed
	}
	src := rating.DefaultPath()
	if !fileExists(src) {
		return
	}
	if err := sandbox.SeedRatings(context.Background(), env, src); err == nil {
		fmt.Fprintf(os.Stderr, "seeded new env %q with learned ratings\n", env)
	}
}

// sandboxLaunch ensures the image exists, then runs the spec, forwarding signals.
func sandboxLaunch(spec sandbox.RunSpec) {
	if !sandbox.ImageExists() {
		fmt.Fprintln(os.Stderr, "sandbox image missing — building it once…")
		sandboxBuild()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := spec.Run(ctx); err != nil {
		// docker propagates the inner exit code; surface it without a scary trace.
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "sandbox error:", err)
		os.Exit(1)
	}
}

// sandboxBuild cross-compiles a linux agent binary for the host's architecture
// and bakes it into the image. Requires the Go toolchain + this module's source
// (the dev/contributor path); OPEN_AGENT_LINUX_BIN overrides with a prebuilt one.
func sandboxBuild() {
	bin := os.Getenv("OPEN_AGENT_LINUX_BIN")
	if bin == "" {
		tmp, err := os.CreateTemp("", "open-agent-linux-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "build error:", err)
			os.Exit(1)
		}
		tmp.Close()
		bin = tmp.Name()
		defer os.Remove(bin)
		fmt.Fprintf(os.Stderr, "cross-compiling linux/%s agent…\n", runtime.GOARCH)
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "cross-compile failed (run from the source tree, or set OPEN_AGENT_LINUX_BIN):", err)
			os.Exit(1)
		}
	}
	if err := sandbox.BuildImage(context.Background(), bin, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "image build failed:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "sandbox image ready:", sandbox.Image)
}

// shellQuote single-quotes a string for safe embedding in the inner `bash -lc`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveEnvFile finds the OpenRouter .env to inject at run time (never baked
// into the image). Mirrors config resolution: env var wins, then ~/.config.
func resolveEnvFile() string {
	if p := os.Getenv("OPEN_AGENT_ENV_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".config", "open-agent", ".env"); fileExists(p) {
		return p
	}
	if fileExists(".env") {
		return ".env"
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

const sandboxUsage = `open-agent sandbox — run the agent as ROOT in a hardened, throwaway container

Usage:
  open-agent sandbox build                       build/refresh the sandbox image
  open-agent sandbox shell  [--env NAME]         root shell in the box
  open-agent sandbox run    [--env NAME] "<task>"  run one agent task inside
  open-agent sandbox exec   [--env NAME] -- CMD  raw command inside the box
  open-agent sandbox seed   [--env NAME] [FILE]  seed the env with host ratings
  open-agent sandbox ls                          list environments
  open-agent sandbox rm     --env NAME           destroy an environment

Flags:
  --env NAME     named environment: its own container + persistent /work volume
                 (default: "default"); distinct envs are fully isolated sessions
  --net MODE     egress: full (default) | none (offline) | https (443+DNS only)
  --ephemeral    do not persist /work (tmpfs; vanishes on exit)
  --no-seed      do not auto-seed a fresh env from host ratings

The agent has full root INSIDE; the host is protected by container isolation
(no --privileged, no host mounts, no docker.sock, no host namespaces, escape-
prone capabilities dropped, pids/memory/cpu capped). The OpenRouter key is
injected at run time via --env-file, never baked into the image.`
