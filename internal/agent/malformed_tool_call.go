package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	malformedToolCallErrorKind            = "malformed_tool_call"
	malformedToolCallOutcomeCorrected     = "corrected"
	malformedToolCallOutcomeUnrecoverable = "unrecoverable"
	malformedToolValidationLabel          = "validation error"
	malformedToolExpectedSchemaLabel      = "expected schema"
	maxMalformedToolCorrectionAttempts    = 1
	malformedToolFailoverCooldown         = 2 * time.Minute
	malformedToolPromptFieldLimit         = 600
)

type malformedToolCallDiagnosis struct {
	ValidationError string
	ExpectedSchema  string
}

func classifyMalformedToolCall(tr ToolResultBlock) (malformedToolCallDiagnosis, bool) {
	if !tr.IsError {
		return malformedToolCallDiagnosis{}, false
	}
	content := strings.TrimSpace(tr.Content)
	if content == "" {
		return malformedToolCallDiagnosis{}, false
	}
	lower := strings.ToLower(content)
	if !strings.Contains(lower, malformedToolValidationLabel) || !strings.Contains(lower, malformedToolExpectedSchemaLabel) {
		return malformedToolCallDiagnosis{}, false
	}
	out := malformedToolCallDiagnosis{
		ValidationError: extractLabeledSection(content, malformedToolValidationLabel, malformedToolExpectedSchemaLabel),
		ExpectedSchema:  extractLabeledSection(content, malformedToolExpectedSchemaLabel),
	}
	if out.ValidationError == "" || out.ExpectedSchema == "" {
		return malformedToolCallDiagnosis{}, false
	}
	return out, true
}

func extractLabeledSection(content, label string, stopLabels ...string) string {
	lower := strings.ToLower(content)
	start := strings.Index(lower, label)
	if start < 0 {
		return ""
	}
	segment := content[start+len(label):]
	segment = strings.TrimPrefix(segment, ":")
	end := len(segment)
	segmentLower := strings.ToLower(segment)
	for _, stop := range stopLabels {
		if idx := strings.Index(segmentLower, stop); idx >= 0 && idx < end {
			end = idx
		}
	}
	return strings.TrimSpace(segment[:end])
}

func buildMalformedToolCorrectionPrompt(tool string, input map[string]any, diag malformedToolCallDiagnosis) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		tool = "tool"
	}
	inputJSON := "{}"
	if len(input) > 0 {
		if data, err := json.Marshal(input); err == nil {
			inputJSON = string(data)
		}
	}
	return strings.TrimSpace(fmt.Sprintf(`Tool-call correction only.
Your previous %s call was rejected by the tool validator.
Validation error:
%s
Expected schema:
%s
Previous input:
%s
Reissue exactly one corrected %s tool call next. No explanation. No other tool first.`,
		tool,
		truncatePromptField(diag.ValidationError),
		truncatePromptField(diag.ExpectedSchema),
		truncatePromptField(inputJSON),
		tool,
	))
}

func truncatePromptField(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= malformedToolPromptFieldLimit {
		return s
	}
	return strings.TrimSpace(s[:malformedToolPromptFieldLimit]) + "\n... (truncated)"
}
