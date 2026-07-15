package agent

import (
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/modeltier"
	providerpkg "github.com/Automaat/sybra/internal/provider"
)

var opencodeDefaultModel = modeltier.Model(modeltier.Cheap, "opencode")

type opencodeProvider struct {
	baseProvider
}

func init() {
	registerAgentProvider(opencodeProvider{})
}

func (opencodeProvider) Name() string { return "opencode" }

func (opencodeProvider) NormalizeModel(model string) string {
	if resolved, ok := modeltier.NormalizeAlias("opencode", model); ok {
		return resolved
	}
	return model
}

func (p opencodeProvider) BuildCommand(cfg RunConfig, model string) string {
	return buildOpenCodeCommand(model, cfg.ReasoningEffort, cfg.Dir, false)
}

func (p opencodeProvider) BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error) {
	args := buildOpenCodeRunArgs(a, cfg, cfg.Prompt)
	return headlessInvocation{
		name:    "opencode",
		args:    args,
		command: "opencode " + strings.Join(args, " "),
	}, nil
}

func (opencodeProvider) ParseHeadlessLine(line []byte) (StreamEvent, error) {
	oe, err := ParseOpenCodeLine(line)
	if err != nil {
		return StreamEvent{}, err
	}
	return opencodeEventToStreamEvent(oe), nil
}

func (opencodeProvider) UsesPerTurnConvo() bool { return true }

func (p opencodeProvider) BuildPerTurnConvoInvocation(a *Agent, cfg RunConfig, prompt string) perTurnConvoInvocation {
	return perTurnConvoInvocation{bin: "opencode", args: buildOpenCodeRunArgs(a, cfg, prompt)}
}

func (opencodeProvider) ParseConvoLine(line []byte) (ConvoEvent, error) {
	oe, err := ParseOpenCodeLine(line)
	if err != nil {
		return ConvoEvent{}, err
	}
	return opencodeEventToConvoEvent(oe), nil
}

func (opencodeProvider) ClassifyError(sample providerpkg.ErrorSample) (providerpkg.Signal, string, time.Duration) {
	return providerpkg.ClassifyOpenCodeError(sample)
}

func buildOpenCodeRunArgs(a *Agent, cfg RunConfig, prompt string) []string {
	args := []string{"run", "--format", "json", "--auto"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = append(args, opencodeReasoningArgs(a.ReasoningEffort)...)
	if a.sessionCWD != "" {
		args = append(args, "--dir", a.sessionCWD)
	} else if cfg.Dir != "" {
		args = append(args, "--dir", cfg.Dir)
	}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--session", sid)
	}
	args = append(args, prompt)
	return args
}

func buildOpenCodeCommand(model, effort, dir string, includePrompt bool) string {
	parts := []string{"opencode", "run", "--format", "json", "--auto"}
	if model != "" {
		parts = append(parts, "--model", model)
	}
	parts = append(parts, opencodeReasoningArgs(effort)...)
	if dir != "" {
		parts = append(parts, "--dir", dir)
	}
	if includePrompt {
		parts = append(parts, "<prompt>")
	}
	return strings.Join(parts, " ")
}

func opencodeReasoningArgs(effort string) []string {
	switch effort {
	case "low", "medium", "high", "xhigh":
		return []string{"--variant", effort}
	default:
		return nil
	}
}
