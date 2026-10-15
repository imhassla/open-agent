package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in: this runs rootful Docker on a disposable native Linux guest, never
// Docker Desktop. Argument inspection alone cannot prove mount enforcement.
func TestNativeSelectedSkillMountsEnforceReadonlyThroughEveryPath(t *testing.T) {
	if os.Getenv("OPEN_AGENT_NATIVE_SKILLS") != "1" {
		t.Skip("requires disposable native Linux Docker")
	}
	root := t.TempDir()
	t.Chdir(root)
	project := filepath.Join(root, ".agents", "skills", "project,fixture")
	helper := filepath.Join(t.TempDir(), "helper")
	for _, base := range []string{project, helper} {
		if err := os.MkdirAll(base, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "resource.txt"), []byte("fixture evidence"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "read.sh"), []byte("#!/bin/sh\ncat \"$(dirname \"$0\")/resource.txt\"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("project,fixture", filepath.Join(root, ".agents", "skills", "alias")); err != nil {
		t.Fatal(err)
	}
	SetSandbox(DockerSandbox{})
	t.Cleanup(func() { SetSandbox(HostSandbox{}) })
	alias, err := MountSkillBundle(project)
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string) string {
		t.Helper()
		out, err := BashExec(context.Background(), command, 30)
		if err != nil || strings.Contains(out, "exit error:") {
			t.Fatalf("shell: %v: %s", err, out)
		}
		return out
	}
	check := func(path string) {
		t.Helper()
		// Real reads distinguish readonly mounts from absent paths. Both resource
		// and script writes must fail, while unrelated project writes still work.
		out := run("sh '" + path + "/read.sh' && if printf changed 2>/tmp/write-error > '" + path + "/resource.txt'; then exit 31; fi; cat /tmp/write-error")
		if !strings.Contains(out, "fixture evidence") || !strings.Contains(out, "Read-only file system") {
			t.Fatalf("mount enforcement at %s: %s", path, out)
		}
		out = run("if printf changed 2>/tmp/write-error > '" + path + "/read.sh'; then exit 32; fi; cat /tmp/write-error")
		if !strings.Contains(out, "Read-only file system") {
			t.Fatalf("script write allowed at %s: %s", path, out)
		}
	}
	check(alias)
	check("/work/.agents/skills/project,fixture")
	check("/work/.agents/skills/alias")
	run("printf writable > /work/unrelated.txt")
	helperAlias, err := MountSkillBundle(helper)
	if err != nil {
		t.Fatal(err)
	}
	check(helperAlias) // helper loaded after earlier shells becomes available
	for _, base := range []string{project, helper} {
		data, err := os.ReadFile(filepath.Join(base, "resource.txt"))
		if err != nil || string(data) != "fixture evidence" {
			t.Fatalf("source changed: %q, %v", data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "unrelated.txt")); err != nil || string(data) != "writable" {
		t.Fatalf("project cwd changed: %q, %v", data, err)
	}
}

func TestNativeSkillMountProtectsWorkingDirectoryInsideBundle(t *testing.T) {
	if os.Getenv("OPEN_AGENT_NATIVE_SKILLS") != "1" {
		t.Skip("requires disposable native Linux Docker")
	}
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{base, filepath.Join(base, "sub")} {
		SetSandbox(DockerSandbox{})
		if _, err := MountSkillBundle(base); err != nil {
			t.Fatal(err)
		}
		out, err := BashExecDir(context.Background(), dir, "pwd; if printf changed 2>/tmp/write-error > /work/changed.txt; then exit 31; fi; cat /tmp/write-error", 30)
		if err != nil || strings.Contains(out, "exit error:") || !strings.Contains(out, "Read-only file system") {
			t.Fatalf("bundle work mount %s: %v: %s", dir, err, out)
		}
	}
	t.Cleanup(func() { SetSandbox(HostSandbox{}) })
}
