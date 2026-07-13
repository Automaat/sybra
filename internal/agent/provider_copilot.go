package agent

import (
	"strings"
	"time"

	providerpkg "github.com/Automaat/sybra/internal/provider"
)

// copilotDefaultModel is the model Copilot agents use when none is specified.
// Per the integration decision the default is the latest GPT model available
// in the installed Copilot binary's registry. If a user's Copilot plan lacks
// this slug, `copilot --model` errors at exec time — pin a newer slug or
// "auto" here when that happens.
const copilotDefaultModel = "gpt-5.5"

type copilotProvider struct {
	baseProvider
}

func init() {
	registerAgentProvider(copilotProvider{})
}

func (copilotProvider) Name() string { return "copilot" }

func (copilotProvider) NormalizeModel(model string) string {
	// The provider-agnostic short aliases (and the empty default the chat
	// path passes) map to the latest GPT. Full Copilot slugs
	// (claude-opus-4.6, gpt-5.5, gemini-3.1-pro-preview, ...) selected
	// in the model picker pass through untouched.
	switch strings.TrimSpace(model) {
	case "", "sonnet", "opus", "haiku", "fable":
		return copilotDefaultModel
	default:
		return model
	}
}

func (copilotProvider) BuildCommand(cfg RunConfig, model string) string {
	return buildCopilotCommand(model, cfg.ReasoningEffort)
}

func (copilotProvider) BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error) {
	// --allow-all-tools is required for non-interactive mode; --no-ask-user
	// stops the agent blocking on questions (no TTY/UI to answer them).
	prompt := stripSkillInvocations(cfg.Prompt, discoverCopilotSkills())
	args := []string{"-p", prompt, "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
	args = append(args, effortArgs(a.ReasoningEffort)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if cfg.MCPConfigJSON != "" {
		mcpJSON, err := wrapMCPConfigWithOwnership(cfg.MCPConfigJSON, mcpOwnerForAgent(a))
		if err != nil {
			return headlessInvocation{}, err
		}
		mcpConfig, err := renderCopilotMCPConfig(mcpJSON)
		if err != nil {
			return headlessInvocation{}, err
		}
		if mcpConfig != "" {
			args = append(args, "--additional-mcp-config", mcpConfig)
		}
	}
	// Copilot only reports its session id on the terminal result event, so
	// a resume id is available only for an intentional stop/restart. Use
	// --session-id (unambiguous required-value flag) over --resume[=value].
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--session-id", sid)
	}
	return headlessInvocation{
		name:    "copilot",
		args:    args,
		command: "copilot " + strings.Join(args, " "),
	}, nil
}

func (copilotProvider) ParseHeadlessLine(line []byte) (StreamEvent, error) {
	ce, err := ParseCopilotLine(line)
	if err != nil {
		return StreamEvent{}, err
	}
	return copilotEventToStreamEvent(ce), nil
}

func (copilotProvider) UsesPerTurnConvo() bool { return true }

func (copilotProvider) BuildPerTurnConvoInvocation(a *Agent, _ RunConfig, prompt string) perTurnConvoInvocation {
	return perTurnConvoInvocation{bin: "copilot", args: buildCopilotConvoArgs(a, prompt)}
}

func (copilotProvider) ParseConvoLine(line []byte) (ConvoEvent, error) {
	ce, err := ParseCopilotLine(line)
	if err != nil {
		return ConvoEvent{}, err
	}
	return copilotEventToConvoEvent(ce), nil
}

func (copilotProvider) ClassifyError(sample providerpkg.ErrorSample) (providerpkg.Signal, string, time.Duration) {
	return providerpkg.ClassifyCopilotError(sample)
}

// buildCopilotCommand builds the display command string for a Copilot agent.
// Headless Copilot always runs --allow-all-tools (required for non-interactive
// mode) and --no-ask-user so it never blocks waiting on a human. The prompt
// (and its `-p` flag) are omitted here — like buildClaudeCommand /
// buildCodexCommand, this is a display-only string showing the flags, not a
// runnable line.
func buildCopilotCommand(model, effort string) string {
	parts := []string{"copilot", "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
	parts = append(parts, effortArgs(effort)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
}
