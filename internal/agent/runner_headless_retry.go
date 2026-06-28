package agent

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/provider"
)

var errProviderRateLimited = errors.New("provider rate-limited")

// reportProviderHealthSignal classifies the final error surface of a failed
// run and forwards rate-limit / auth failures to the provider health gate so
// the next scheduling attempt can fail over to a peer.
func (m *Manager) reportProviderHealthSignal(a *Agent, stderrOut string, attemptEvents []StreamEvent) provider.Signal {
	sample := buildErrorSample(stderrOut, attemptEvents)
	return m.reportProviderHealthSample(a, sample)
}

func (m *Manager) reportCleanProviderHealthSignal(a *Agent, stderrOut string, attemptEvents []StreamEvent) provider.Signal {
	sample := buildErrorSample(stderrOut, attemptEvents)
	sample.ContentIsCleanResult = true
	return m.reportProviderHealthSample(a, sample)
}

func (m *Manager) reportProviderHealthSample(a *Agent, sample provider.ErrorSample) provider.Signal {
	sig, reason, retryAfter := classifyProviderError(a.Provider, sample)
	if sig == provider.SignalNone {
		if sample.ErrorType != "" || sample.ErrorStatus != 0 {
			m.logger.Info("agent.provider.signal.unknown",
				"provider", a.Provider,
				"errorType", sample.ErrorType,
				"errorStatus", sample.ErrorStatus)
		}
		return sig
	}
	// Record the classification on the agent so the completion handler can tell
	// a transient provider limit apart from a real crash and retry instead of
	// stranding the task in human-required.
	a.SetError(signalErrorKind(sig), reason)
	m.ReportProviderSignal(a.Provider, sig, reason, retryAfter)
	return sig
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
func (m *Manager) reportProviderHealthSignalConvo(a *Agent, stderrOut string, attemptEvents []ConvoEvent) provider.Signal {
	sample := buildErrorSampleConvo(stderrOut, attemptEvents)
	return m.reportProviderHealthSample(a, sample)
}

func (m *Manager) reportCleanProviderHealthSignalConvo(a *Agent, stderrOut string, attemptEvents []ConvoEvent) provider.Signal {
	sample := buildErrorSampleConvo(stderrOut, attemptEvents)
	sample.ContentIsCleanResult = true
	return m.reportProviderHealthSample(a, sample)
}

// classifyProviderError routes an error sample to the provider-appropriate
// classifier. Without a copilot branch a logged-out / quota-exhausted copilot
// would never be flagged, leaving the health gate routing failover work to it.
func classifyProviderError(prov string, sample provider.ErrorSample) (provider.Signal, string, time.Duration) {
	p, err := lookupProvider(prov)
	if err != nil {
		return provider.SignalNone, "", 0
	}
	return p.ClassifyError(sample)
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
	if slices.ContainsFunc(streamEvents, retryableResultEvent) {
		return true
	}
	// Substring fallback: keeps working if Anthropic changes the error envelope.
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	if slices.ContainsFunc(streamEvents, func(e StreamEvent) bool {
		return resultEventCanCarryError(e) && substringMatch529(e.Content)
	}) {
		warnSubstringFallback(logger)
		return true
	}
	return false
}

// shouldRetryConvo is the ConvoEvent variant of shouldRetry.
func shouldRetryConvo(stderrOut string, convoEvents []ConvoEvent, logger *slog.Logger) bool {
	if slices.ContainsFunc(convoEvents, retryableConvoResultEvent) {
		return true
	}
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	if slices.ContainsFunc(convoEvents, func(e ConvoEvent) bool {
		return convoResultEventCanCarryError(e) && substringMatch529(e.Text)
	}) {
		warnSubstringFallback(logger)
		return true
	}
	return false
}

func retryableResultEvent(e StreamEvent) bool {
	return e.Type == "result" && (e.ErrorType == "overloaded_error" || e.ErrorStatus == 529)
}

func resultEventCanCarryError(e StreamEvent) bool {
	return e.Type == "result" && (resultSubtypeIsError(e.Subtype) || e.ErrorType != "" || e.ErrorStatus != 0)
}

func retryableConvoResultEvent(e ConvoEvent) bool {
	return e.Type == "result" && (e.ErrorType == "overloaded_error" || e.ErrorStatus == 529)
}

func convoResultEventCanCarryError(e ConvoEvent) bool {
	return e.Type == "result" && (resultSubtypeIsError(e.Subtype) || e.ErrorType != "" || e.ErrorStatus != 0)
}

func resultSubtypeIsError(subtype string) bool {
	return subtype != "" && subtype != "success"
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
