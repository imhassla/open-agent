package tools

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectedSkillBundlesUseReadonlyAliasesAndProjectOverlays(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	project := filepath.Join(root, ".agents", "skills", "project,fixture")
	personal := filepath.Join(t.TempDir(), "personal")
	for _, dir := range []string{project, personal} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cli := t.TempDir()
	capture := filepath.Join(cli, "args")
	if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", capture)
	SetSandbox(DockerSandbox{Image: "fixture:local"})
	t.Cleanup(func() { SetSandbox(HostSandbox{}) })
	alias, err := MountSkillBundle(project)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(alias, "/skills/") {
		t.Fatalf("execution alias = %q", alias)
	}
	again, err := MountSkillBundle(project)
	if err != nil || again != alias {
		t.Fatalf("unstable alias: %q, %v", again, err)
	}
	if _, err := BashExec(context.Background(), "true", 5); err != nil {
		t.Fatal(err)
	}
	// A helper loaded after a shell invocation is included in the next invocation.
	helper, err := MountSkillBundle(personal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BashExec(context.Background(), "true", 5); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	mounts := make(map[string][]string)
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			fields, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range fields {
				if strings.HasPrefix(field, "target=") {
					mounts[strings.TrimPrefix(field, "target=")] = fields
				}
			}
		}
	}
	for _, target := range []string{alias, helper, "/work/.agents/skills/project,fixture"} {
		fields := mounts[target]
		if len(fields) == 0 {
			t.Errorf("missing mount for %q: %v", target, args)
			continue
		}
		readonly := false
		for _, field := range fields {
			if field == "readonly" {
				readonly = true
			}
		}
		if !readonly {
			t.Errorf("mount %q is writable: %v", target, fields)
		}
	}
	if len(mounts["/work"]) == 0 {
		t.Fatal("project working directory mount missing")
	}
}
