// Package llmexec runs short structured-output LLM CLI calls with provider
// failover. It is for classifiers/judges/planners, not long-running coding
// agents (those go through internal/agent.Manager).
package llmexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/provider"
)

var providerOrder = []string{"claude", "codex", "copilot"}

const streamScannerBuffer = 4 * 1024 * 1024

// errSchemaDelivery wraps failures creating/writing the codex output-schema
// temp file. RunJSON treats it as a failover-eligible provider failure rather
// than a hard error, so a codex-local filesystem issue falls back to the next
// provider instead of aborting the whole call.
var errSchemaDelivery = errors.New("schema delivery failed")

// Options configures a one-shot provider invocation.
type Options struct {
	// Provider is the preferred provider. Empty means claude first, then peers.
	Provider string
	// Models maps provider name to the model slug passed to that provider's CLI.
	Models map[string]string
	// DisableTools runs the call with no tool access instead of the default
	// full-tool bypass. Use for classifiers/summarizers whose prompt embeds
	// any text a prior model turn authored — without this, that text runs in
	// a tool-enabled session and a prompt-injected instruction could act on
	// it. Claude denies every tool (--disallowedTools "*"); other providers
	// are invoked unchanged (RunJSON has no tool-enabled peer path today).
	DisableTools bool
	Logger       *slog.Logger
	Gate         provider.HealthGate
	// Schema is an optional JSON Schema describing the expected result shape.
	// Codex receives it natively via `--output-schema <tempfile>` (no prose);
	// claude and copilot have no such flag, so it is embedded as prose in the
	// prompt instead. Empty means no schema is delivered by either path.
	Schema string
}

// Result is the normalized final assistant text.
type Result struct {
	Provider string
	Text     string
	// CostUSD is the provider-reported spend for this call. Only populated
	// for claude, whose --output-format json envelope carries total_cost_usd;
	// codex/copilot leave this zero.
	CostUSD float64
}

// RunJSON runs prompt against the preferred provider and falls back to peers
// when the provider is unavailable, unhealthy, logged out, or rate-limited.
func RunJSON(ctx context.Context, prompt string, opts Options) (Result, error) {
	candidates := candidates(opts.Provider)
	var failures []string
	for _, p := range candidates {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if opts.Gate != nil && !opts.Gate.IsHealthy(p) {
			failures = append(failures, fmt.Sprintf("%s: unhealthy (%s)", p, opts.Gate.Reason(p)))
			continue
		}
		if _, err := exec.LookPath(binaryName(p)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: CLI not found", p))
			continue
		}

		raw, stderrOut, err := runProvider(ctx, p, prompt, opts.Models[p], opts.DisableTools, opts.Schema)
		if err != nil {
			if errors.Is(err, errSchemaDelivery) {
				failures = append(failures, fmt.Sprintf("%s: %s", p, err))
				continue
			}
			if overloaded(stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalRateLimit, "overloaded")
				failures = append(failures, fmt.Sprintf("%s: overloaded", p))
				continue
			}
			sig, reason, retryAfter := classifyError(p, stderrOut, string(raw))
			if sig != provider.SignalNone {
				reportSignal(opts.Gate, p, sig, reason, retryAfter)
				logFallback(opts.Logger, p, sig, reason)
				failures = append(failures, fmt.Sprintf("%s: %s", p, reason))
				continue
			}
			return Result{}, providerError(p, err, stderrOut)
		}

		text, cost, parseErr := parseProviderText(p, raw)
		if parseErr != nil {
			if overloaded(stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalRateLimit, "overloaded")
				failures = append(failures, fmt.Sprintf("%s: overloaded", p))
				continue
			}
			sig, reason, retryAfter := classifyError(p, stderrOut, string(raw))
			if sig != provider.SignalNone {
				reportSignal(opts.Gate, p, sig, reason, retryAfter)
				logFallback(opts.Logger, p, sig, reason)
				failures = append(failures, fmt.Sprintf("%s: %s", p, reason))
				continue
			}
			return Result{}, fmt.Errorf("%s output: %w", p, parseErr)
		}
		return Result{Provider: p, Text: text, CostUSD: cost}, nil
	}
	if len(failures) == 0 {
		return Result{}, errors.New("no providers configured")
	}
	return Result{}, fmt.Errorf("all providers failed: %s", strings.Join(failures, "; "))
}

func candidates(preferred string) []string {
	preferred = normalizeProvider(preferred)
	if preferred == "" {
		return slices.Clone(providerOrder)
	}
	out := []string{preferred}
	for _, p := range providerOrder {
		if p != preferred {
			out = append(out, p)
		}
	}
	return out
}

func normalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", "claude":
		return "claude"
	case "codex":
		return "codex"
	case "copilot":
		return "copilot"
	default:
		return ""
	}
}

func binaryName(p string) string {
	if p == "copilot" {
		return "copilot"
	}
	return p
}

func runProvider(ctx context.Context, p, prompt, model string, disableTools bool, schema string) (stdout []byte, stderrOut string, err error) {
	effectivePrompt := prompt
	schemaPath := ""
	if strings.TrimSpace(schema) != "" {
		if p == "codex" {
			path, schemaErr := writeSchemaTempFile(schema)
			if schemaErr != nil {
				return nil, "", fmt.Errorf("%w: %w", errSchemaDelivery, schemaErr)
			}
			defer os.Remove(path)
			schemaPath = path
		} else {
			effectivePrompt = prompt + "\n\nOutput schema:\n" + strings.TrimSpace(schema)
		}
	}

	name, args, stdin := invocation(p, effectivePrompt, model, disableTools, schemaPath)
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.String(), err
}

func writeSchemaTempFile(schema string) (string, error) {
	f, err := os.CreateTemp("", "sybra-llmexec-schema-*.json")
	if err != nil {
		return "", fmt.Errorf("create schema temp file: %w", err)
	}
	if _, err := f.WriteString(schema); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write schema temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close schema temp file: %w", err)
	}
	return f.Name(), nil
}

func invocation(p, prompt, model string, disableTools bool, schemaPath string) (name string, args []string, stdin string) {
	switch p {
	case "codex":
		args := []string{
			"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
			"--dangerously-bypass-approvals-and-sandbox",
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		if schemaPath != "" {
			args = append(args, "--output-schema", schemaPath)
		}
		args = append(args, "-")
		return "codex", args, prompt
	case "copilot":
		args := []string{"-p", prompt, "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
		if model != "" {
			args = append(args, "--model", model)
		}
		return "copilot", args, ""
	default:
		args := []string{"-p", prompt, "--output-format", "json"}
		if disableTools {
			args = append(args, "--disallowedTools", "*")
		} else {
			args = append(args, "--dangerously-skip-permissions")
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		return "claude", args, ""
	}
}

func parseProviderText(p string, raw []byte) (text string, costUSD float64, err error) {
	switch p {
	case "codex":
		text, err := parseCodexText(raw)
		return text, 0, err
	case "copilot":
		text, err := parseCopilotText(raw)
		return text, 0, err
	default:
		return parseClaudeText(raw)
	}
}

func parseClaudeText(raw []byte) (text string, costUSD float64, err error) {
	var env struct {
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", 0, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if strings.TrimSpace(env.Result) == "" {
		return "", 0, errors.New("empty result field")
	}
	return env.Result, env.TotalCostUSD, nil
}

func parseCodexText(raw []byte) (string, error) {
	var final string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, streamScannerBuffer), streamScannerBuffer)
	for scanner.Scan() {
		var ev struct {
			Type      string `json:"type"`
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
			Code      int    `json:"code"`
			Item      *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return "", err
		}
		if ev.Type == "error" {
			return "", fmt.Errorf("provider error %s (%d): %s", ev.ErrorType, ev.Code, ev.Message)
		}
		if ev.Item != nil && strings.TrimSpace(ev.Item.Text) != "" {
			final = ev.Item.Text
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(final) == "" {
		return "", errors.New("no assistant message in codex output")
	}
	return final, nil
}

func parseCopilotText(raw []byte) (string, error) {
	var final string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, streamScannerBuffer), streamScannerBuffer)
	for scanner.Scan() {
		var ev struct {
			Type      string `json:"type"`
			Ephemeral bool   `json:"ephemeral"`
			ExitCode  int    `json:"exitCode"`
			Data      *struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return "", err
		}
		if ev.Type == "result" && ev.ExitCode != 0 {
			return "", fmt.Errorf("provider exit code %d", ev.ExitCode)
		}
		if ev.Type == "assistant.message" && !ev.Ephemeral && ev.Data != nil && strings.TrimSpace(ev.Data.Content) != "" {
			final = ev.Data.Content
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(final) == "" {
		return "", errors.New("no assistant message in copilot output")
	}
	return final, nil
}

func classifyError(p, stderrOut, content string) (provider.Signal, string, time.Duration) {
	sample := provider.ErrorSample{Stderr: stderrOut, Content: content}
	switch p {
	case "codex":
		return provider.ClassifyCodexError(sample)
	case "copilot":
		return provider.ClassifyCopilotError(sample)
	default:
		return provider.ClassifyClaudeError(sample)
	}
}

func overloaded(parts ...string) bool {
	for _, p := range parts {
		lower := strings.ToLower(p)
		if strings.Contains(lower, "529") || strings.Contains(lower, "overloaded") {
			return true
		}
	}
	return false
}

func reportSignal(g provider.HealthGate, p string, sig provider.Signal, reason string, retryAfter time.Duration) {
	if g == nil {
		return
	}
	switch sig {
	case provider.SignalAuthFailure:
		g.ReportAuthFailure(p, reason)
	case provider.SignalRateLimit:
		g.ReportRateLimit(p, retryAfter, reason)
	case provider.SignalNone:
	}
}

func logFallback(logger *slog.Logger, p string, sig provider.Signal, reason string) {
	if logger == nil {
		return
	}
	logger.Warn("llmexec.provider_fallback", "from", p, "signal", sig, "reason", reason)
}

func providerError(p string, err error, stderrOut string) error {
	msg := strings.TrimSpace(stderrOut)
	if msg == "" {
		return fmt.Errorf("%s: %w", p, err)
	}
	return fmt.Errorf("%s: %w: %s", p, err, msg)
}
