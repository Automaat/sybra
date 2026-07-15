package workflow

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/skills"
	"gopkg.in/yaml.v3"
)

type skillPromptFrontmatter struct {
	Name                   string `yaml:"name"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

func TestBuiltinInternalWorkflowPromptsDoNotImplicitlySelectOperatorSkills(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		workflow string
		step     string
		skill    string
		needle   string
	}{
		{name: "plan_prompt_skips_sybra_plan", workflow: "simple-task-plan", step: "plan", skill: "sybra-plan", needle: "Plan task"},
		{name: "plan_prompt_skips_sybra_tasks", workflow: "simple-task-plan", step: "plan", skill: "sybra-tasks", needle: "Run: sybra-cli --json get"},
		{name: "review_prompt_skips_sybra_tasks", workflow: "simple-task-review", step: "code_review_staff", skill: "sybra-tasks", needle: "Task.ID"},
		{name: "test_prompt_skips_sybra_tasks", workflow: "testing-task", step: "run_test", skill: "sybra-tasks", needle: "Adversarially test task"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			def := mustBuiltinDefinition(t, tc.workflow)
			step := def.StepByID(tc.step)
			if step == nil {
				t.Fatalf("%s step %s not found", tc.workflow, tc.step)
			}
			prompt := step.Config.Prompt
			if prompt == "" {
				t.Fatalf("%s/%s prompt is empty", tc.workflow, tc.step)
			}
			if !strings.Contains(prompt, tc.needle) {
				t.Fatalf("%s/%s prompt missing precondition %q", tc.workflow, tc.step, tc.needle)
			}
			if strings.Contains(prompt, "/"+tc.skill) || strings.Contains(prompt, "$"+tc.skill) {
				t.Fatalf("%s/%s prompt explicitly invokes %s", tc.workflow, tc.step, tc.skill)
			}

			meta := mustPromptSkillFrontmatter(t, tc.skill)
			if !meta.DisableModelInvocation {
				t.Fatalf("%s must disable implicit model invocation for internal workflow prompts", tc.skill)
			}
		})
	}
}

func mustPromptSkillFrontmatter(t *testing.T, name string) skillPromptFrontmatter {
	t.Helper()

	data, err := skills.FS.ReadFile("data/" + name + ".md")
	if err != nil {
		t.Fatalf("read %s skill: %v", name, err)
	}
	block := promptFrontmatterBlock(t, string(data))
	var meta skillPromptFrontmatter
	if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
		t.Fatalf("parse %s frontmatter: %v", name, err)
	}
	return meta
}

func promptFrontmatterBlock(t *testing.T, text string) string {
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
