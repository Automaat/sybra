package agent

import (
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/modeltier"
	providerpkg "github.com/Automaat/sybra/internal/provider"
)

type codexProvider struct{}

func init() {
	registerAgentProvider(codexProvider{})
}

func (codexProvider) Name() string { return "codex" }

func (codexProvider) NormalizeModel(model string) string {
	// Codex models come from `codex debug models` and never carry a [1m]
	// suffix — a stray suffix stays untouched and is rejected by safeArgRe.
	if resolved, ok := modeltier.NormalizeAlias("codex", model); ok {
		return resolved
	}
	return model
}

func (p codexProvider) BuildCommand(cfg RunConfig, model string) string {
	return buildCodexCommand(model, cfg.ReasoningEffort, cfg.RequirePermissions, cfg.Mode == "headless", p)
}

func (p codexProvider) BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error) {
	skillNames := discoverCodexSkills()
	prompt := rewriteSkillInvocations(cfg.Prompt, skillNames)
	args := codexExecBaseArgs(prompt != cfg.Prompt)
	// headless=true: --sandbox workspace-write requires approval prompts
	// which auto-reject in headless mode (no TTY/UI). Always bypass.
	args = append(args, p.SandboxArgs(cfg.RequirePermissions, true)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = append(args, codexReasoningArgs(a.ReasoningEffort)...)
	if a.sessionCWD != "" {
		args = append(args, "-C", a.sessionCWD)
	}
	if cfg.outputSchemaPath != "" {
		args = append(args, "--output-schema", cfg.outputSchemaPath)
	}
	// Lifecycle hooks (fail-open: omitted when sybra-cli unresolvable or taskID unsafe).
	args = append(args, buildCodexHookArgs(a.TaskID)...)
	if cfg.MCPConfigJSON != "" {
		mcpJSON, err := wrapMCPConfigWithOwnership(cfg.MCPConfigJSON, mcpOwnerForAgent(a))
		if err != nil {
			return headlessInvocation{}, err
		}
		mcpArgs, err := renderCodexMCPArgs(mcpJSON)
		if err != nil {
			return headlessInvocation{}, err
		}
		args = append(args, mcpArgs...)
	}
	args = append(args, prompt)
	return headlessInvocation{
		name:    "codex",
		args:    args,
		command: "codex " + strings.Join(args, " "),
	}, nil
}

func codexExecBaseArgs(loadUserConfig bool) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	if !loadUserConfig {
		args = append(args, "--ignore-user-config")
	}
	args = append(args, "--ignore-rules")
	return args
}

func (codexProvider) ParseHeadlessLine(line []byte) (StreamEvent, error) {
	ce, err := ParseCodexLine(line)
	if err != nil {
		return StreamEvent{}, err
	}
	return codexEventToStreamEvent(ce), nil
}

func (codexProvider) SandboxArgs(requirePerms, headless bool) []string {
	if os.Getenv("SYBRA_DISABLE_CODEX_SANDBOX") == "1" {
		return []string{"--sandbox", "danger-full-access"}
	}
	if !requirePerms || headless {
		// Bypass all approval prompts and sandbox restrictions.
		// For headless runs this is required regardless of RequirePermissions:
		// --sandbox workspace-write auto-rejects approval requests since there
		// is no TTY/UI to serve them, which silently breaks the agent run.
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	}
	return []string{"--sandbox", "workspace-write"}
}

func (codexProvider) OutputSchemaAsFile() bool { return true }

func (codexProvider) UsesPerTurnConvo() bool { return true }

func (p codexProvider) BuildPerTurnConvoInvocation(a *Agent, cfg RunConfig, prompt string) perTurnConvoInvocation {
	return perTurnConvoInvocation{bin: "codex", args: buildCodexConvoArgsWithProvider(a, cfg, prompt, p)}
}

func (codexProvider) ParseConvoLine(line []byte) (ConvoEvent, error) {
	ce, err := ParseCodexLine(line)
	if err != nil {
		return ConvoEvent{}, err
	}
	return codexEventToConvoEvent(ce), nil
}

func (codexProvider) SessionFilePath(sessionID string) string {
	return resolveCodexSessionFile(sessionID)
}

func (codexProvider) ClassifyError(sample providerpkg.ErrorSample) (providerpkg.Signal, string, time.Duration) {
	return providerpkg.ClassifyCodexError(sample)
}

// buildCodexCommand builds the display command string for a Codex agent.
func buildCodexCommand(model, effort string, requirePerms, headless bool, p Provider) string {
	parts := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
	parts = append(parts, p.SandboxArgs(requirePerms, headless)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	parts = append(parts, codexReasoningArgs(effort)...)
	return strings.Join(parts, " ")
}

// codexReasoningArgs returns the codex config override for model reasoning
// effort, or nil when effort is empty (model default). Centralizing the
// empty-value no-op here keeps all three codex builders from each re-deriving
// it — a bare -c model_reasoning_effort= (empty value) is NOT the same as
// omitting the flag.
func codexReasoningArgs(effort string) []string {
	if effort == "" {
		return nil
	}
	// Allowlist guards against tampered task YAML bypassing API-layer validation.
	switch effort {
	case "low", "medium", "high", "xhigh":
	default:
		return nil
	}
	return []string{"-c", "model_reasoning_effort=" + effort}
}

// codexSandboxArgs returns the sandbox/permission flags for `codex exec`.
// When SYBRA_DISABLE_CODEX_SANDBOX=1 is set, the bwrap-backed sandbox is
// replaced with `--sandbox danger-full-access`. Required when running
// inside a Docker/LXC container whose kernel blocks unprivileged user
// namespaces (kernel.unprivileged_userns_clone=0), where bwrap crashes
// before the agent can execute any command.
//
// headless must be true for headless runs. --sandbox workspace-write asks
// for user approval on writes outside the workspace; in headless mode there
// is no UI to serve those approval prompts, so they auto-reject and the run
// fails. Bypass mode is used instead.
func codexSandboxArgs(requirePerms, headless bool) []string {
	return providerByName("codex").SandboxArgs(requirePerms, headless)
}
