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

	"github.com/Automaat/sybra/internal/errclass"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/textutil"
)

var providerOrder = providerid.All()

const streamScannerBuffer = 4 * 1024 * 1024

// providerWaitDelay bounds how long Wait blocks after ctx cancellation before
// force-closing the provider's output pipes. Kept well under App.Shutdown's
// grace so a one-shot provider call can never be the goroutine that outlives
// shutdown.
const providerWaitDelay = 5 * time.Second

// errSchemaDelivery wraps failures creating/writing the codex output-schema
// temp file. RunJSON treats it as a failover-eligible provider failure rather
// than a hard error, so a codex-local filesystem issue falls back to the next
// provider instead of aborting the whole call.
var errSchemaDelivery = errors.New("schema delivery failed")

// errWorkdir wraps failures preparing the call's working directory. Treated
// like errSchemaDelivery: a local filesystem problem is the host's, not this
// provider's, but failing the whole call over it would take down a classifier
// that another provider could still answer.
var errWorkdir = errors.New("working directory unavailable")

// Options configures a one-shot provider invocation.
type Options struct {
	// Provider is the preferred provider. Empty means claude first, then peers.
	Provider string
	// Models maps provider name to the model slug passed to that provider's CLI.
	Models map[string]string
	// EnableTools gives the call tool access. It is off by default: these are
	// classifiers, judges and planners that answer from their prompt. The
	// default used to be the opposite, which put a fully-permissioned CLI
	// behind prompts built from GitHub issue and pull-request text (#3383).
	//
	// One caller sets it. agent.Inspect hands the model a log path and tells
	// it to read the tail, and a tools-off run there returns hallucinated
	// tool-call markup — which the watchdog only logs, so the stall detector
	// would go quietly dead. Weigh a new true against that: it is the bar,
	// not a formality.
	//
	// Off means claude denies every tool, codex runs read-only, and copilot
	// is not given blanket tool permission. Providers differ in what they
	// offer here, so the process sandbox below is the containment that does
	// not depend on a CLI honouring a flag.
	EnableTools bool
	// Dir is the working directory for the CLI. Empty means a fresh empty
	// directory per call, removed afterwards — never the serving process's
	// cwd, which is a source checkout on the deploy host.
	Dir    string
	Logger *slog.Logger
	Gate   provider.HealthGate
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
	candidates := candidates(opts.Provider, opts.EnableTools)
	var failures []string
	var lastProvider string
	for _, p := range candidates {
		lastProvider = p
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

		raw, stderrOut, err := runProvider(ctx, p, prompt, opts.Models[p], opts, opts.Schema)
		if err != nil {
			if errors.Is(err, errSchemaDelivery) || errors.Is(err, errWorkdir) {
				failures = append(failures, fmt.Sprintf("%s: %s", p, err))
				continue
			}
			if schemaFlagRejected(p, opts.Schema, stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalNone, "unsupported_output_schema")
				failures = append(failures, fmt.Sprintf("%s: unsupported output-schema flag", p))
				continue
			}
			if overloaded(stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalRateLimit, "overloaded")
				failures = append(failures, fmt.Sprintf("%s: overloaded", p))
				continue
			}
			c := classifyError(p, stderrOut, string(raw))
			if c.Signal != provider.SignalNone {
				reportSignal(opts.Gate, p, c)
				logFallback(opts.Logger, p, c.Signal, c.Reason)
				failures = append(failures, fmt.Sprintf("%s: %s", p, c.Reason))
				continue
			}
			return Result{Provider: p}, providerError(p, err, stderrOut, string(raw))
		}

		text, cost, parseErr := parseProviderText(p, raw)
		if parseErr != nil {
			if schemaFlagRejected(p, opts.Schema, stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalNone, "unsupported_output_schema")
				failures = append(failures, fmt.Sprintf("%s: unsupported output-schema flag", p))
				continue
			}
			if overloaded(stderrOut, string(raw)) {
				logFallback(opts.Logger, p, provider.SignalRateLimit, "overloaded")
				failures = append(failures, fmt.Sprintf("%s: overloaded", p))
				continue
			}
			c := classifyError(p, stderrOut, string(raw))
			if c.Signal != provider.SignalNone {
				reportSignal(opts.Gate, p, c)
				logFallback(opts.Logger, p, c.Signal, c.Reason)
				failures = append(failures, fmt.Sprintf("%s: %s", p, c.Reason))
				continue
			}
			return Result{Provider: p}, fmt.Errorf("%s output: %w", p, parseErr)
		}
		return Result{Provider: p, Text: text, CostUSD: cost}, nil
	}
	if len(failures) == 0 {
		return Result{}, errors.New("no providers configured")
	}
	return Result{Provider: lastProvider}, fmt.Errorf("all providers failed: %s", strings.Join(failures, "; "))
}

// candidates orders the failover chain, dropping any provider that cannot run
// with tools off when the call asked for that. A chain whose last hop quietly
// restores tool access is worse than a shorter chain: the caller reads one
// guarantee from Options and gets another from whichever provider answered.
func candidates(preferred string, enableTools bool) []string {
	preferred = normalizeProvider(preferred)
	out := make([]string, 0, len(providerOrder)+1)
	if preferred != "" {
		out = append(out, preferred)
	}
	for _, p := range providerOrder {
		if p != preferred {
			out = append(out, p)
		}
	}
	if enableTools {
		return out
	}
	// An explicit preference is honoured — the caller named that CLI and can
	// read this guarantee for itself. What is dropped is the silent fallback:
	// a chain that ends at a tool-enabled provider hands back a different
	// guarantee than the one the caller set, and nothing in the result says so.
	return slices.DeleteFunc(out, func(p string) bool {
		return p != preferred && !supportsToolsOff(p)
	})
}

// supportsToolsOff reports whether this CLI has a flag that denies tool use.
// claude denies every tool, codex runs read-only, copilot is simply not given
// blanket permission. opencode's non-interactive mode is --auto, which
// approves every call, and it offers no verified alternative.
func supportsToolsOff(p string) bool {
	return p != providerid.OpenCode
}

func normalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", providerid.Claude:
		return providerid.Claude
	case providerid.Codex:
		return providerid.Codex
	case providerid.Copilot:
		return providerid.Copilot
	case providerid.OpenCode:
		return providerid.OpenCode
	default:
		return ""
	}
}

func binaryName(p string) string {
	if p == providerid.Copilot {
		return providerid.Copilot
	}
	return p
}

func runProvider(ctx context.Context, p, prompt, model string, opts Options, schema string) (stdout []byte, stderrOut string, err error) {
	effectivePrompt := prompt
	schemaPath := ""
	if strings.TrimSpace(schema) != "" {
		if p == providerid.Codex {
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

	name, args, stdin := invocation(p, effectivePrompt, model, opts.EnableTools, schemaPath)
	cmd, release, err := providerCommand(ctx, opts, name, args)
	if err != nil {
		return nil, "", err
	}
	defer release()
	// Without WaitDelay, cancelling ctx kills the provider CLI but Wait still
	// blocks until every write end of the output pipe closes — a grandchild
	// that outlives its parent holds it open indefinitely, so the exec
	// survives shutdown and keeps writing into SYBRA_HOME. Same failure the
	// agent runners already guard against.
	cmd.WaitDelay = providerWaitDelay
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

func invocation(p, prompt, model string, enableTools bool, schemaPath string) (name string, args []string, stdin string) {
	switch p {
	case providerid.Codex:
		args := []string{
			"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		}
		if enableTools {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "--sandbox", "read-only")
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		if schemaPath != "" {
			args = append(args, "--output-schema", schemaPath)
		}
		args = append(args, "-")
		return providerid.Codex, args, prompt
	case providerid.Copilot:
		args := []string{"-p", prompt, "--output-format", "json", "--no-ask-user"}
		if enableTools {
			args = append(args, "--allow-all-tools")
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		return providerid.Copilot, args, ""
	case providerid.OpenCode:
		// --auto approves every tool call. There is no verified no-tool flag
		// for this CLI, so candidates() drops opencode entirely when tools
		// are off; reaching here means the caller asked for them.
		args := []string{"run", "--format", "json", "--auto"}
		if model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, prompt)
		return providerid.OpenCode, args, ""
	default:
		args := []string{"-p", prompt, "--output-format", "json"}
		if enableTools {
			args = append(args, "--dangerously-skip-permissions")
		} else {
			args = append(args, "--disallowedTools", "*")
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		return providerid.Claude, args, ""
	}
}

func parseProviderText(p string, raw []byte) (text string, costUSD float64, err error) {
	switch p {
	case providerid.Codex:
		text, err := parseCodexText(raw)
		return text, 0, err
	case providerid.Copilot:
		text, err := parseCopilotText(raw)
		return text, 0, err
	case providerid.OpenCode:
		text, err := parseOpenCodeText(raw)
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

func parseOpenCodeText(raw []byte) (string, error) {
	var final string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, streamScannerBuffer), streamScannerBuffer)
	for scanner.Scan() {
		var ev struct {
			Type    string          `json:"type"`
			Content string          `json:"content"`
			Text    string          `json:"text"`
			Message string          `json:"message"`
			Error   string          `json:"error"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return "", err
		}
		if strings.Contains(strings.ToLower(ev.Type), "error") {
			msg := firstNonEmpty(ev.Error, ev.Message, ev.Text, ev.Content)
			if msg == "" {
				msg = ev.Type
			}
			return "", errors.New(msg)
		}
		if strings.Contains(strings.ToLower(ev.Type), "assistant") {
			if text := firstNonEmpty(ev.Content, ev.Text, ev.Message, openCodeDataText(ev.Data)); text != "" {
				final = text
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(final) == "" {
		return "", errors.New("no assistant message in opencode output")
	}
	return final, nil
}

func openCodeDataText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var data struct {
		Content string `json:"content"`
		Text    string `json:"text"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return firstNonEmpty(data.Content, data.Text, data.Message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func classifyError(p, stderrOut, content string) provider.Classification {
	sample := provider.ErrorSample{Stderr: stderrOut, Content: content}
	switch p {
	case providerid.Codex:
		return provider.ClassifyCodexError(sample)
	case providerid.Copilot:
		return provider.ClassifyCopilotError(sample)
	case providerid.OpenCode:
		return provider.ClassifyOpenCodeError(sample)
	default:
		return provider.ClassifyClaudeError(sample)
	}
}

func overloaded(parts ...string) bool {
	for _, p := range parts {
		if errclass.Classify(p, errclass.LLMExecRecoveryBiased) == errclass.RateLimited {
			return true
		}
	}
	return false
}

func schemaFlagRejected(providerName, schema string, parts ...string) bool {
	if providerName != providerid.Codex || strings.TrimSpace(schema) == "" {
		return false
	}
	for _, part := range parts {
		lower := strings.ToLower(part)
		if !strings.Contains(lower, "--output-schema") {
			continue
		}
		if containsAnyString(lower,
			"unknown option",
			"unknown flag",
			"unknown argument",
			"unrecognized option",
			"unrecognized argument",
			"unexpected option",
			"unexpected argument",
			"found argument '--output-schema' which wasn't expected",
			"no such option",
			"unsupported option",
		) {
			return true
		}
	}
	return false
}

func containsAnyString(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func reportSignal(g provider.HealthGate, p string, c provider.Classification) {
	if g == nil {
		return
	}
	switch c.Signal {
	case provider.SignalAuthFailure:
		g.ReportAuthFailure(p, c.Reason)
	case provider.SignalRateLimit:
		g.ReportRateLimit(p, c.RetryAfter, c.Reason, c.Source)
	case provider.SignalNone:
	}
}

func logFallback(logger *slog.Logger, p string, sig provider.Signal, reason string) {
	if logger == nil {
		return
	}
	logger.Warn("llmexec.provider_fallback", "from", p, "signal", sig, "reason", reason)
}

func providerError(p string, err error, stderrOut, stdoutOut string) error {
	msg := strings.TrimSpace(stderrOut)
	if msg == "" {
		msg = providerStdoutDetail(stdoutOut)
	}
	if msg == "" {
		return fmt.Errorf("%s: %w", p, err)
	}
	return fmt.Errorf("%s: %w: %s", p, err, msg)
}

// providerStdoutDetail salvages a reason from stdout for a CLI that reports
// the failure there and exits with an empty stderr — codex prints the API's
// rejection as a JSON error event, so "exit status 1" is otherwise all the
// operator ever sees.
func providerStdoutDetail(stdoutOut string) string {
	for line := range strings.SplitSeq(stdoutOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, `"error"`) {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		detail := firstNonEmpty(event.Error.Message, event.Message)
		if detail == "" {
			continue
		}
		return textutil.TruncateBytesTrimmed(strings.ToValidUTF8(detail, ""), providerDetailMax, "...")
	}
	return ""
}

// providerDetailMax bounds the salvaged stdout reason: an API rejection can
// echo the whole schema back, and this error reaches a task status reason.
const providerDetailMax = 400
