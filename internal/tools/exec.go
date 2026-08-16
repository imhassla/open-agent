package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// maxBashBytes caps captured command output so a runaway command can't flood the
// context window or exhaust memory.
const maxBashBytes = 64 << 10

// Sandbox runs a shell command. The default runs on the host; a Docker sandbox
// can isolate network and cap resources for untrusted generated code. RunInDir
// runs in a specific working directory (empty dir == process cwd); Run is the
// cwd-relative convenience.
type Sandbox interface {
	Run(ctx context.Context, command string, timeoutSec int) (string, error)
	RunInDir(ctx context.Context, dir, command string, timeoutSec int) (string, error)
}

// active is the process-wide sandbox, set once at startup before workers run.
var active Sandbox = HostSandbox{}

// SetSandbox swaps the active sandbox.
func SetSandbox(s Sandbox) {
	if s != nil {
		active = s
	}
}

// BashExec runs a command through the active sandbox. A non-zero exit is reported
// in the returned text (not as a Go error) so the agent can read and react to it.
func BashExec(ctx context.Context, command string, timeoutSec int) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	return active.Run(ctx, command, timeoutSec)
}

// BashExecDir is BashExec with an explicit working directory (used by the
// FAIL_TO_PASS baseline gate, which runs against a throwaway HEAD checkout).
func BashExecDir(ctx context.Context, dir, command string, timeoutSec int) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	return active.RunInDir(ctx, dir, command, timeoutSec)
}

// StreamRunner is the optional capability of a sandbox that can tee output to a
// live writer as it runs (not just return it on completion). The host sandbox
// implements it; a sandbox that can't stream simply isn't asserted to it and
// callers fall back to the buffered BashExec.
type StreamRunner interface {
	RunStream(ctx context.Context, command string, timeoutSec int, w io.Writer) (string, error)
}

// BashExecStream runs a command and, when the active sandbox supports streaming,
// tees combined stdout/stderr to w live while still returning the captured
// (capped) result. When the active sandbox can't stream (e.g. Docker), it falls
// back to the buffered BashExec — same result, just no live output.
func BashExecStream(ctx context.Context, command string, timeoutSec int, w io.Writer) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	if sr, ok := active.(StreamRunner); ok && w != nil {
		return sr.RunStream(ctx, command, timeoutSec, w)
	}
	return active.Run(ctx, command, timeoutSec)
}

// RunStream implements StreamRunner for the host sandbox.
func (HostSandbox) RunStream(ctx context.Context, command string, timeoutSec int, w io.Writer) (string, error) {
	return execCaptureStream(ctx, timeoutSec, "", w, "bash", "-c", command)
}

// HostSandbox runs commands directly on the host (default).
type HostSandbox struct{}

func (h HostSandbox) Run(ctx context.Context, command string, timeoutSec int) (string, error) {
	return h.RunInDir(ctx, "", command, timeoutSec)
}

func (HostSandbox) RunInDir(ctx context.Context, dir, command string, timeoutSec int) (string, error) {
	return execCapture(ctx, timeoutSec, dir, "bash", "-c", command)
}

// DockerSandbox runs commands in a throwaway container with no network and
// resource caps, bind-mounting the working directory read-write so file edits
// persist. Requires docker on PATH (verify with DockerAvailable).
type DockerSandbox struct {
	Image  string // default alpine:3.20
	CPUs   string // default "2"
	Memory string // default "2g"
}

func (d DockerSandbox) Run(ctx context.Context, command string, timeoutSec int) (string, error) {
	return d.RunInDir(ctx, "", command, timeoutSec)
}

func (d DockerSandbox) RunInDir(ctx context.Context, dir, command string, timeoutSec int) (string, error) {
	mountDir := dir
	if mountDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		mountDir = wd
	}
	image := d.Image
	if image == "" {
		image = "alpine:3.20"
	}
	cpus := d.CPUs
	if cpus == "" {
		cpus = "2"
	}
	mem := d.Memory
	if mem == "" {
		mem = "2g"
	}
	return execCapture(ctx, timeoutSec, "",
		"docker", "run", "--rm", "--network", "none", "--cpus", cpus, "--memory", mem,
		"-v", mountDir+":/work", "-w", "/work", image, "sh", "-lc", command)
}

// DockerAvailable reports whether the docker CLI is on PATH.
func DockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// groupCmd builds a command that runs in its OWN process group so a timeout can
// kill the whole tree — background jobs (`&`), pipelines and subshells — not just
// the top-level shell. Without this a leaked child keeps the output pipe open and
// the wait blocks long past the deadline (the "hangs after timeout_sec" bug).
func groupCmd(cctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir // empty => inherit the process working directory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative PID signals the whole process group led by the child.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return os.ErrProcessDone
	}
	// Backstop: if a grandchild still holds the pipe after the group kill, don't
	// wait on it forever — abandon the I/O copy shortly after cancellation.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// finalizeResult applies the shared result formatting (cap, timeout/exit
// wrapping, empty-output marker) so the buffered and streaming paths agree.
func finalizeResult(raw string, timedOut bool, timeoutSec int, runErr error) (string, error) {
	res := capOutput(strings.TrimSpace(raw))
	if timedOut {
		return res, fmt.Errorf("command timed out after %ds", timeoutSec)
	}
	if runErr != nil {
		return fmt.Sprintf("exit error: %v\n--- output ---\n%s", runErr, res), nil
	}
	if res == "" {
		res = "(command produced no output)"
	}
	return res, nil
}

func execCapture(ctx context.Context, timeoutSec int, dir, name string, args ...string) (string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := groupCmd(cctx, dir, name, args...)
	out, err := cmd.CombinedOutput()
	return finalizeResult(string(out), cctx.Err() == context.DeadlineExceeded, timeoutSec, err)
}

// execCaptureStream is execCapture that ALSO tees combined stdout/stderr to w as
// it runs, so the caller sees live progress. It still captures the full output
// and returns the identical (capped, error-wrapped) result.
func execCaptureStream(ctx context.Context, timeoutSec int, dir string, w io.Writer, name string, args ...string) (string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := groupCmd(cctx, dir, name, args...)
	var buf bytes.Buffer
	mw := io.MultiWriter(&buf, w) // capture + live
	cmd.Stdout = mw
	cmd.Stderr = mw

	var runErr error
	if err := cmd.Start(); err != nil {
		runErr = err
	} else {
		runErr = cmd.Wait()
	}
	return finalizeResult(buf.String(), cctx.Err() == context.DeadlineExceeded, timeoutSec, runErr)
}

// capOutput keeps the head and tail of oversized output with a marker between.
func capOutput(s string) string {
	if len(s) <= maxBashBytes {
		return s
	}
	half := maxBashBytes / 2
	return fmt.Sprintf("%s\n…[%d bytes capped]…\n%s", s[:half], len(s)-maxBashBytes, s[len(s)-half:])
}
