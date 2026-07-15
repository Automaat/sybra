package skills_test

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/skills"
	"gopkg.in/yaml.v3"
)

type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	UserInvocable          bool   `yaml:"user-invocable"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

func TestOperatorSkillsStayUserInvocableButDisableImplicitInvocation(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"sybra-plan", "sybra-tasks"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			meta := mustSkillFrontmatter(t, name)
			if !meta.UserInvocable {
				t.Fatalf("%s must stay explicitly invocable by users", name)
			}
			if !meta.DisableModelInvocation {
				t.Fatalf("%s must disable implicit model invocation", name)
			}
			if !strings.Contains(meta.Description, "human explicitly asks") {
				t.Fatalf("%s description must stay explicit-invocation scoped: %q", name, meta.Description)
			}
			if !strings.Contains(meta.Description, "Do not use for workflow-dispatched") {
				t.Fatalf("%s description must exclude workflow-dispatched agents: %q", name, meta.Description)
			}
		})
	}
}

func mustSkillFrontmatter(t *testing.T, name string) skillFrontmatter {
	t.Helper()

	data, err := skills.FS.ReadFile("data/" + name + ".md")
	if err != nil {
		t.Fatalf("read %s skill: %v", name, err)
	}
	block := frontmatterBlock(t, string(data))
	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
		t.Fatalf("parse %s frontmatter: %v", name, err)
	}
	return meta
}

func frontmatterBlock(t *testing.T, text string) string {
	t.Helper()

	trimmed := strings.TrimLeft(text, " \t\r\n\ufeff")
	rest, ok := strings.CutPrefix(trimmed, "---\n")
	if !ok {
		t.Fatal("missing YAML frontmatter start")
	}
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		t.Fatal("missing YAML frontmatter end")
	}
	return block
}
