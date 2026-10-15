package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestNativeGuestSkillProbe(t *testing.T) {
	if os.Getenv("OPEN_AGENT_SKILL_PROBE") != "1" {
		t.Skip("guest-only probe")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	catalog := skills.Discover(cwd, home)
	for _, name := range []string{"guest-project", "guest-personal"} {
		view, err := catalog.View(name, "", 0, 0)
		if err != nil || !strings.Contains(view.Content, "Guest-local evidence.") {
			t.Fatalf("%s: %+v, %v", name, view, err)
		}
	}
	if _, err := catalog.Resolve("host-only"); err == nil {
		t.Fatal("host skill leaked into whole-agent sandbox")
	}
}

func TestNativeWholeAgentSkillDiscoveryUsesGuestFilesOnly(t *testing.T) {
	if os.Getenv("OPEN_AGENT_NATIVE_SKILLS") != "1" {
		t.Skip("requires disposable native Linux Docker")
	}
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	writeSkill := func(base, name string) {
		t.Helper()
		dir := filepath.Join(base, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Native fixture\n---\nGuest-local evidence.\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(hostHome, "host-only")
	build := t.TempDir()
	writeSkill(filepath.Join(build, "project"), "guest-project")
	writeSkill(filepath.Join(build, "home"), "guest-personal")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "probe"), data, 0755); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM ubuntu:24.04\nCOPY probe /probe\nCOPY project /work/project\nCOPY home/.agents /work/.agents\nENV OPEN_AGENT_SKILL_PROBE=1\nENTRYPOINT [\"/bin/bash\"]\n"
	if err := os.WriteFile(filepath.Join(build, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", args[0], err, out)
		}
		return string(out)
	}
	image := "open-agent-skills-native-fixture:e2cb"
	run("build", "-q", "-t", image, build)
	t.Cleanup(func() { run("image", "rm", image) })
	env := "skills-native-fixture"
	t.Cleanup(func() { run("volume", "rm", VolumeName(env)) })
	args := RunSpec{Env: env, Persist: true, Network: NetNone, Limits: DefaultLimits(), Cmd: []string{"cd /work/project && /probe -test.v -test.run '^TestNativeGuestSkillProbe$'"}}.RunArgs()
	// Use a static probe image with the production RunSpec isolation flags.
	// No host paths are mounted; the named work volume starts with image files.
	for i, arg := range args {
		if arg == Image {
			args[i] = image
		}
	}
	out := run(args...)
	if !strings.Contains(out, "--- PASS: TestNativeGuestSkillProbe") {
		t.Fatalf("guest qualification did not execute: %s", out)
	}
	t.Log(out)
}
