package skillattr

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	Unknown = "unknown"
	None    = "none"

	ExecutionModeUnknown     = Unknown
	ExecutionModeNone        = None
	ExecutionModeNative      = "native"
	ExecutionModeInjected    = "injected"
	ExecutionModeFallback    = "fallback"
	ExecutionModeUnavailable = "unavailable"

	ConformanceUnknown     = Unknown
	ConformanceNone        = None
	ConformanceExact       = "exact"
	ConformanceFallback    = "fallback"
	ConformanceUnavailable = "unavailable"
)

func NormalizeExecutionMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case ExecutionModeNone,
		ExecutionModeNative,
		ExecutionModeInjected,
		ExecutionModeFallback,
		ExecutionModeUnavailable:
		return strings.TrimSpace(mode)
	default:
		return ExecutionModeUnknown
	}
}

func NormalizeConformance(state string) string {
	switch strings.TrimSpace(state) {
	case ConformanceNone,
		ConformanceExact,
		ConformanceFallback,
		ConformanceUnavailable:
		return strings.TrimSpace(state)
	default:
		return ConformanceUnknown
	}
}

func ConformanceForExecutionMode(mode string) string {
	switch NormalizeExecutionMode(mode) {
	case ExecutionModeNone:
		return ConformanceNone
	case ExecutionModeNative, ExecutionModeInjected:
		return ConformanceExact
	case ExecutionModeFallback:
		return ConformanceFallback
	case ExecutionModeUnavailable:
		return ConformanceUnavailable
	default:
		return ConformanceUnknown
	}
}

func HashSourceID(sourceID string) string {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sourceID))
	return hex.EncodeToString(sum[:8])
}
