package agent

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/provider"
)

// reportProviderHealthSignal classifies the final error surface of a failed
// run and forwards rate-limit / auth failures to the provider health gate so
// the next scheduling attempt can fail over to a peer.
func (m *Manager) reportProviderHealthSignal(a *Agent, stderrOut string, attemptEvents []StreamEvent) {
	sample := buildErrorSample(stderrOut, attemptEvents)
	var sig provider.Signal
	var reason string
	var retryAfter time.Duration
	if a.Provider == "codex" {
		sig, reason, retryAfter = provider.ClassifyCodexError(sample)
	} else {
		sig, reason, retryAfter = provider.ClassifyClaudeError(sample)
	}
	if sig == provider.SignalNone {
		if sample.ErrorType != "" || sample.ErrorStatus != 0 {
			m.logger.Info("agent.provider.signal.unknown",
				"provider", a.Provider,
				"errorType", sample.ErrorType,
				"errorStatus", sample.ErrorStatus)
		}
		return
	}
	// Record the classification on the agent so the completion handler can tell
	// a transient provider limit apart from a real crash and retry instead of
	// stranding the task in human-required.
	a.SetError(signalErrorKind(sig), reason)
	m.ReportProviderSignal(a.Provider, sig, reason, retryAfter)
}

// signalErrorKind maps a provider health signal to the short error-kind tag
// recorded on the agent (consumed by the completion handler).
func signalErrorKind(sig provider.Signal) string {
	switch sig {
	case provider.SignalRateLimit:
		return "rate_limit"
	case provider.SignalAuthFailure:
		return "auth"
	default:
		return ""
	}
}

func buildErrorSample(stderrOut string, attemptEvents []StreamEvent) provider.ErrorSample {
	sample := provider.ErrorSample{Stderr: stderrOut}
	for i := range slices.Backward(attemptEvents) {
		e := &attemptEvents[i]
		if e.Type != "result" {
			continue
		}
		// Capture the terminal result regardless of subtype. A provider usage
		// cap (e.g. a five-hour session limit) is reported on a subtype:"success"
		// result with the limit text in Content — not a structured error
		// envelope — so a subtype=="error" filter would miss it.
		sample.ErrorType = e.ErrorType
		sample.ErrorStatus = e.ErrorStatus
		sample.Content = e.Content
		break
	}
	return sample
}

// reportProviderHealthSignalConvo mirrors reportProviderHealthSignal for the
// ConvoEvent stream used by conversational runners.
func (m *Manager) reportProviderHealthSignalConvo(a *Agent, stderrOut string, attemptEvents []ConvoEvent) {
	sample := buildErrorSampleConvo(stderrOut, attemptEvents)
	var sig provider.Signal
	var reason string
	var retryAfter time.Duration
	if a.Provider == "codex" {
		sig, reason, retryAfter = provider.ClassifyCodexError(sample)
	} else {
		sig, reason, retryAfter = provider.ClassifyClaudeError(sample)
	}
	if sig == provider.SignalNone {
		if sample.ErrorType != "" || sample.ErrorStatus != 0 {
			m.logger.Info("agent.provider.signal.unknown",
				"provider", a.Provider,
				"errorType", sample.ErrorType,
				"errorStatus", sample.ErrorStatus)
		}
		return
	}
	a.SetError(signalErrorKind(sig), reason)
	m.ReportProviderSignal(a.Provider, sig, reason, retryAfter)
}

func buildErrorSampleConvo(stderrOut string, attemptEvents []ConvoEvent) provider.ErrorSample {
	sample := provider.ErrorSample{Stderr: stderrOut}
	for i := range slices.Backward(attemptEvents) {
		e := &attemptEvents[i]
		if e.Type != "result" {
			continue
		}
		// Capture the terminal result regardless of subtype (see buildErrorSample).
		sample.ErrorType = e.ErrorType
		sample.ErrorStatus = e.ErrorStatus
		sample.Content = e.Text
		break
	}
	return sample
}

// shouldRetry returns true when stderrOut or streamEvents indicate an Anthropic
// 529 (overloaded) transient error that warrants a backoff retry.
//
// Structured fields on StreamEvent (ErrorType, ErrorStatus) are checked first.
// Substring matching is used as a fallback and triggers a Warn log so format
// regressions surface in logs without silently breaking retries.
func shouldRetry(stderrOut string, streamEvents []StreamEvent, logger *slog.Logger) bool {
	for i := range streamEvents {
		if streamEvents[i].Type == "result" && streamEvents[i].Subtype == "error" {
			if streamEvents[i].ErrorType == "overloaded_error" || streamEvents[i].ErrorStatus == 529 {
				return true
			}
		}
	}
	// Substring fallback: keeps working if Anthropic changes the error envelope.
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	for i := range streamEvents {
		if streamEvents[i].Type == "result" && streamEvents[i].Subtype == "error" && substringMatch529(streamEvents[i].Content) {
			warnSubstringFallback(logger)
			return true
		}
	}
	return false
}

// shouldRetryConvo is the ConvoEvent variant of shouldRetry.
func shouldRetryConvo(stderrOut string, convoEvents []ConvoEvent, logger *slog.Logger) bool {
	for i := range convoEvents {
		e := &convoEvents[i]
		if e.Type == "result" && e.Subtype == "error" {
			if e.ErrorType == "overloaded_error" || e.ErrorStatus == 529 {
				return true
			}
		}
	}
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	for i := range convoEvents {
		e := &convoEvents[i]
		if e.Type == "result" && e.Subtype == "error" && substringMatch529(e.Text) {
			warnSubstringFallback(logger)
			return true
		}
	}
	return false
}

func substringMatch529(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "529") || strings.Contains(lower, "overloaded")
}

func warnSubstringFallback(logger *slog.Logger) {
	if logger != nil {
		logger.Warn("agent.retry.substring-fallback",
			"hint", "structured error fields absent; check if Anthropic changed error format")
	}
}
