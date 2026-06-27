package agent

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/google/uuid"
)

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	// Hard guard: every agent must run in an explicit, existing directory.
	// An empty Dir means the spawned process inherits Sybra's cwd, which in
	// dev mode is the Sybra source repo — agents would then mutate its
	// branches via git checkout. Reject rather than silently leak.
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("agent.Run: Dir is required (empty Dir would leak agent process into Sybra cwd)")
	}
	if info, err := os.Stat(cfg.Dir); err != nil {
		return nil, fmt.Errorf("agent.Run: Dir %q not accessible: %w", cfg.Dir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("agent.Run: Dir %q is not a directory", cfg.Dir)
	}

	// Inject the worktree's working-memory scratchpad (NOTES.md): a standing
	// read/maintain instruction plus the file's current contents, giving Codex
	// (no --resume) and any restarted agent cross-run continuity. Gated on
	// SeedWorkingMemory, set only for code-author roles: verifier roles (review,
	// test-runner, eval) reuse the SAME per-task worktree, so seeding them would
	// feed an independent reviewer/tester the implementer's notes and quietly
	// erode the reward-hacking defense. Done on the local cfg copy so the inlined
	// notes never reach the persisted AgentRun.Prompt, which callers record from
	// their own prompt variable. No-op when Dir has no NOTES.md.
	if cfg.SeedWorkingMemory {
		cfg.Prompt = notes.SeedPrompt(cfg.Prompt, cfg.Dir)
	}

	resolvedProvider, gateErr := m.gateProvider(cfg)
	if gateErr != nil {
		return nil, gateErr
	}

	m.mu.RLock()
	if cfg.BashTimeoutMs == 0 {
		cfg.BashTimeoutMs = m.bashTimeoutMs
	}
	if cfg.RetryWatchdog == 0 {
		cfg.RetryWatchdog = m.retryWatchdog
	}
	if cfg.FallbackModel == "" {
		cfg.FallbackModel = m.fallbackModel
	}
	m.mu.RUnlock()

	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	now := time.Now().UTC()
	a := &Agent{
		ID:                     id,
		TaskID:                 cfg.TaskID,
		Name:                   cfg.Name,
		Mode:                   cfg.Mode,
		Provider:               resolvedProvider,
		Model:                  normalizeModel(resolvedProvider, cfg.Model),
		ReasoningEffort:        cfg.ReasoningEffort,
		Prompt:                 cfg.Prompt,
		State:                  StateRunning,
		StartedAt:              now,
		LastEventAt:            now,
		cancel:                 cancel,
		sessionCWD:             cfg.Dir,
		MaxTurns:               cfg.MaxTurns,
		requirePermissions:     cfg.RequirePermissions,
		headlessPermissionMode: cfg.HeadlessPermissionMode,
	}
	if cfg.ResumeSessionID != "" {
		a.SetSessionID(cfg.ResumeSessionID)
	}
	if cfg.Mode == "headless" || cfg.Mode == "interactive" {
		a.done = make(chan struct{})
	}
	if cfg.Mode == "headless" {
		a.escalationCh = make(chan bool, 1)
	}
	// Pre-mark detached so a shutdown racing the runner goroutine already
	// knows to leave this agent's record alive. Headless and interactive
	// Claude survive as detached processes; codex interactive has no
	// persistent process and survives by recreate-on-restart, but is still
	// marked so ShutdownWithGrace leaves its record for recreate.
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	m.mu.Lock()
	if !cfg.IgnoreConcurrencyLimit && m.maxConcurrent > 0 && m.liveCount >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("max concurrent agents reached (%d)", m.maxConcurrent)
	}
	m.agents[id] = a
	if a.done != nil {
		m.liveCount++
	}
	m.mu.Unlock()

	metrics.AgentStarted(a.Provider, a.Mode)
	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "provider", a.Provider, "model", a.Model)

	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg)
	case "interactive":
		// codex and copilot use the per-turn conversational runner (each turn
		// spawns a fresh process); claude uses the persistent approval-hook
		// runner. Copilot's permission model is CLI-flag based (no HTTP
		// approval hook), so the per-turn shape fits it like codex.
		if a.Provider == "codex" || a.Provider == "copilot" {
			a.promptCh = make(chan string, 1)
			go m.runPerTurnConversational(ctx, a, cfg, false)
		} else {
			a.approvalCh = make(chan ApprovalResponse, 1)
			go m.runConversational(ctx, a, cfg)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", cfg.Mode)
	}

	m.emit(events.AgentState(id), a)
	return a, nil
}

func (m *Manager) markAgentDone(a *Agent) {
	if a == nil || a.done == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
		// Close the stdin pipe/FIFO and drop the registry record + FIFO file
		// so a completed agent leaks neither an fd nor an on-disk pipe.
		a.stdinMu.Lock()
		if a.stdinPipe != nil {
			_ = a.stdinPipe.Close()
			a.stdinPipe = nil
		}
		a.stdinMu.Unlock()
		if reg := m.registry(); reg != nil {
			_ = reg.Delete(a.ID)
		}
		m.mu.Lock()
		if m.liveCount > 0 {
			m.liveCount--
		}
		m.mu.Unlock()
	})
}

func (m *Manager) buildCommand(cfg RunConfig) (string, error) {
	prov := m.providerForRun(cfg.Provider)
	model := normalizeModel(prov, cfg.Model)
	if model != "" && !safeArgRe.MatchString(model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}
	switch prov {
	case "codex":
		return buildCodexCommand(model, cfg.ReasoningEffort, cfg.RequirePermissions, cfg.Mode == "headless"), nil
	case "copilot":
		return buildCopilotCommand(model), nil
	default:
		return buildClaudeCommand(model, cfg.AllowedTools, cfg.RequirePermissions, cfg.HeadlessPermissionMode), nil
	}
}

// buildClaudeCommand builds the display command string for a Claude agent.
func buildClaudeCommand(model string, allowedTools []string, requirePerms bool, mode string) string {
	parts := []string{"claude"}
	parts = append(parts, claudePermissionArgs(allowedTools, requirePerms, mode)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
}

// buildCodexCommand builds the display command string for a Codex agent.
func buildCodexCommand(model, effort string, requirePerms, headless bool) string {
	parts := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
	parts = append(parts, codexSandboxArgs(requirePerms, headless)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	parts = append(parts, codexReasoningArgs(effort)...)
	return strings.Join(parts, " ")
}

// buildCopilotCommand builds the display command string for a Copilot agent.
// Headless Copilot always runs --allow-all-tools (required for non-interactive
// mode) and --no-ask-user so it never blocks waiting on a human. The prompt
// (and its `-p` flag) are omitted here — like buildClaudeCommand /
// buildCodexCommand, this is a display-only string showing the flags, not a
// runnable line.
func buildCopilotCommand(model string) string {
	parts := []string{"copilot", "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
	if model != "" {
		parts = append(parts, "--model", model)
	}
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

// gateProvider resolves the run's provider through the health gate. If the
// configured provider is unhealthy and auto-failover can supply a healthy
// peer, the peer is returned. Otherwise returns a typed UnhealthyError so
// callers can detect via errors.Is(err, provider.ErrProviderUnhealthy).
func (m *Manager) gateProvider(cfg RunConfig) (string, error) {
	resolved := m.providerForRun(cfg.Provider)
	if cfg.IgnoreHealthGate {
		return resolved, nil
	}
	m.mu.RLock()
	g := m.gate
	m.mu.RUnlock()
	if g == nil {
		return resolved, nil
	}
	if g.IsHealthy(resolved) {
		return resolved, nil
	}
	if alt := g.Failover(resolved); alt != "" {
		metrics.AgentFailover(resolved, alt)
		m.logger.Warn("agent.run.failover", "from", resolved, "to", alt, "task", cfg.TaskID, "reason", g.Reason(resolved))
		return alt, nil
	}
	reason := g.Reason(resolved)
	metrics.AgentGated(resolved, reason)
	return "", &provider.UnhealthyError{
		Provider: resolved,
		Reason:   reason,
	}
}

func (m *Manager) providerForRun(name string) string {
	m.mu.RLock()
	def := m.defaultProv
	m.mu.RUnlock()
	if name == "" {
		name = def
	}
	return normalizeProvider(name)
}

func normalizeProvider(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "claude":
		return "claude"
	case "codex":
		return "codex"
	case "copilot":
		return "copilot"
	default:
		return "claude"
	}
}

// copilotDefaultModel is the model Copilot agents use when none is specified.
// Per the integration decision the default is the latest GPT model available
// in the installed Copilot binary's registry. If a user's Copilot plan lacks
// this slug, `copilot --model` errors at exec time — pin a newer slug or
// "auto" here when that happens.
const copilotDefaultModel = "gpt-5.4"

func normalizeModel(prov, model string) string {
	switch normalizeProvider(prov) {
	case "codex":
		// Codex models come from `codex debug models` and never carry a [1m]
		// suffix — a stray suffix stays untouched and is rejected by safeArgRe.
		switch strings.TrimSpace(model) {
		case "", "sonnet", "opus", "fable":
			return "gpt-5.5"
		case "haiku":
			return "gpt-5.4-mini"
		default:
			return model
		}
	case "copilot":
		// The provider-agnostic short aliases (and the empty default the chat
		// path passes) map to the latest GPT. Full Copilot slugs
		// (claude-sonnet-4.6, gpt-5.3-codex, gemini-3-pro-preview, …) selected
		// in the model picker pass through untouched.
		switch strings.TrimSpace(model) {
		case "", "sonnet", "opus", "haiku", "fable":
			return copilotDefaultModel
		default:
			return model
		}
	default:
		// [1m] is a Claude-Code-only context marker. Fable 5 ships a 1M context
		// window by default, so CC 2.1.173 strips the redundant suffix; Sybra
		// exposes no 1M variants, so the marker is always redundant here.
		// Stripping before safeArgRe keeps the validator strict — it intentionally
		// rejects '[' and ']'. Scoped to the Claude path; Codex strings untouched.
		model = stripContextSuffix(model)
		if strings.TrimSpace(model) == "" {
			return "sonnet"
		}
		return model
	}
}

var oneMSuffixRe = regexp.MustCompile(`(?i)\[1m\]$`)

func stripContextSuffix(model string) string {
	return oneMSuffixRe.ReplaceAllString(strings.TrimSpace(model), "")
}

// safeArgRe matches only characters safe to embed in a shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
