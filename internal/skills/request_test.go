package skills_test

import (
	"os"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestRequestsAnywhereComposeDistinctReferenceBlocksInOrder(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for _, name := range []string{"review", "refactor", "help"} {
		bundle(t, project, name, "---\ndescription: "+name+" procedure\ndisable-model-invocation: true\n---\n"+name+" instructions\n")
	}
	c := skills.Discover(project, home)
	for _, text := range []string{"/review examine changes", "examine /review changes", "examine changes /review", "Please use review after editing", "Explain review; do not use it"} {
		p, err := c.Prepare(skills.Request{Instructions: text, Context: "generated /refactor"})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Names) != 1 || p.Names[0] != "review" || p.Instructions != text {
			t.Fatalf("request %q = %+v", text, p)
		}
		if !strings.Contains(p.Reference, "reference context") || !strings.Contains(p.Reference, "review instructions") {
			t.Fatalf("reference = %q", p.Reference)
		}
	}
	p, err := c.Prepare(skills.Request{Instructions: "/refactor then /skill:review and /refactor"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Names, ",") != "refactor,review" || strings.Index(p.Reference, "refactor instructions") > strings.Index(p.Reference, "review instructions") {
		t.Fatalf("ordered request = %+v", p)
	}
	for _, text := range []string{"https://site/review", "./review.md", "/tmp/review", "/review.md", "/help"} {
		p, err := c.Prepare(skills.Request{Instructions: text})
		if err != nil || len(p.Names) != 0 {
			t.Fatalf("path/builtin %q = %+v, %v", text, p, err)
		}
	}
	p, err = c.Prepare(skills.Request{Instructions: "/skill:help"})
	if err != nil || len(p.Names) != 1 || p.Names[0] != "help" {
		t.Fatalf("namespaced collision = %+v, %v", p, err)
	}
	if _, err := c.Prepare(skills.Request{Instructions: "After editing, /skill:missing"}); err == nil {
		t.Fatal("missing explicit skill accepted")
	}
	p, err = c.Prepare(skills.Request{Instructions: "ordinary task", Context: "Use /review"})
	if err != nil || len(p.Names) != 0 {
		t.Fatalf("generated context became user request = %+v, %v", p, err)
	}
}

func TestRequiredNamedContextFailsInsteadOfSilentlyTruncating(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, project, "large", "---\ndescription: Large\n---\n"+strings.Repeat("A procedure line.\n", 4000))
	_, err := skills.Discover(project, home).Prepare(skills.Request{Instructions: "Use /large"})
	if err == nil || !strings.Contains(err.Error(), "required named skill context exceeds") {
		t.Fatalf("context limit = %v", err)
	}
}

func TestAcceptedNonconventionalNamesKeepExplicitNamespacedInvocation(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for _, name := range []string{"review.v2", "review:helper", "révision", "?"} {
		bundle(t, project, name, "---\ndescription: Fixture\n---\nFixture instructions.")
	}
	catalog := skills.Discover(project, home)
	for _, name := range []string{"review.v2", "review:helper", "révision", "?"} {
		for _, prompt := range []string{"After editing, use /skill:" + name, "After editing, use (`/skill:" + name + "`)."} {
			prepared, err := catalog.Prepare(skills.Request{Instructions: prompt})
			if err != nil || len(prepared.Names) != 1 || prepared.Names[0] != name {
				t.Fatalf("namespaced %s: %+v, %v", name, prepared, err)
			}
		}
	}
}

func TestExistingRootPathsDoNotBecomeMissingSkillRequests(t *testing.T) {
	catalog := skills.Discover(t.TempDir(), t.TempDir())
	for _, path := range []string{"/tmp", "/etc"} {
		if _, err := os.Lstat(path); err != nil {
			t.Skipf("root path fixture unavailable: %s: %v", path, err)
		}
		for _, text := range []string{"Explain " + path, "Read configuration under (" + path + ")."} {
			prepared, err := catalog.Prepare(skills.Request{Instructions: text})
			if err != nil || len(prepared.Names) != 0 {
				t.Errorf("root path %q became a skill request: %+v, %v", text, prepared, err)
			}
		}
	}
	for _, text := range []string{"Use /skill:tmp", "Use /open-agent-missing-skill-fixture"} {
		if _, err := catalog.Prepare(skills.Request{Instructions: text}); err == nil {
			t.Errorf("missing explicit skill %q accepted", text)
		}
	}
	project := t.TempDir()
	bundle(t, project, "tmp", "---\ndescription: Fixture\ndisable-model-invocation: true\n---\nTmp skill instructions.")
	catalog = skills.Discover(project, t.TempDir())
	for _, text := range []string{"Use /tmp", "Use /skill:tmp"} {
		prepared, err := catalog.Prepare(skills.Request{Instructions: text})
		if err != nil || len(prepared.Names) != 1 || prepared.Names[0] != "tmp" {
			t.Errorf("known skill %q lost precedence: %+v, %v", text, prepared, err)
		}
	}
}

func TestSlashRequestsUseUnicodeWhitespaceBoundaries(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, project, "review", "---\ndescription: Fixture\ndisable-model-invocation: true\n---\nReview instructions.")
	catalog := skills.Discover(project, home)
	for _, space := range []string{"\u00a0", "\u202f", "\u3000", "\v"} {
		for _, prompt := range []string{"Use" + space + "/review", "/review" + space + "after editing", "After editing," + space + "/skill:review"} {
			prepared, err := catalog.Prepare(skills.Request{Instructions: prompt})
			if err != nil || len(prepared.Names) != 1 || prepared.Names[0] != "review" || prepared.Instructions != prompt {
				t.Errorf("Unicode request %q lost explicit context: %+v, %v", prompt, prepared, err)
			}
		}
	}
}
