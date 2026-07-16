package agent

import (
	"encoding/json"
	"hash/fnv"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type toolLoopObservation struct {
	signature string
	label     string
}

type semanticToolPart struct {
	signature string
	label     string
}

func toolLoopObservationForUses(toolUses []ToolUseBlock) toolLoopObservation {
	if len(toolUses) == 0 {
		return toolLoopObservation{}
	}

	parts := make([]semanticToolPart, 0, len(toolUses))
	for i := range toolUses {
		part := semanticToolPartForUse(toolUses[i])
		if part.signature == "" {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return toolLoopObservation{}
	}

	sigParts := make([]string, 0, len(parts))
	labels := make([]string, 0, len(parts))
	for i := range parts {
		sigParts = append(sigParts, parts[i].signature)
		labels = append(labels, parts[i].label)
	}
	slices.Sort(sigParts)
	slices.Sort(labels)
	return toolLoopObservation{
		signature: hashParts(sigParts),
		label:     strings.Join(compactUnique(labels), " + "),
	}
}

// toolSignature returns the semantic action-family fingerprint for one
// assistant turn's tool calls, or "" when there are no tool calls.
func toolSignature(toolUses []ToolUseBlock) string {
	return toolLoopObservationForUses(toolUses).signature
}

func semanticToolPartForUse(use ToolUseBlock) semanticToolPart {
	name := strings.TrimSpace(use.Name)
	if name == "" {
		return semanticToolPart{}
	}
	switch name {
	case "Bash":
		cmd, _ := use.Input["command"].(string)
		label := normalizeBashActionLabel(cmd)
		if label == "" {
			label = "bash"
		}
		return semanticToolPart{signature: label, label: label}
	case "Read":
		if path := normalizeInputPath(use.Input); path != "" {
			label := "read:" + path
			return semanticToolPart{signature: label, label: label}
		}
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		if path := normalizeInputPath(use.Input); path != "" {
			label := "edit:" + path
			return semanticToolPart{signature: label, label: label}
		}
	}

	var b strings.Builder
	b.WriteString(name)
	if use.Input != nil {
		if raw, err := json.Marshal(use.Input); err == nil {
			b.Write(raw)
		}
	}
	label := strings.ToLower(strings.TrimSpace(name))
	if label == "" {
		label = "tool"
	}
	return semanticToolPart{signature: b.String(), label: label}
}

func normalizeBashActionLabel(command string) string {
	cmd := normalizeShellWords(command)
	if cmd == "" {
		return ""
	}
	pipeline := splitShellPipeline(cmd)
	for len(pipeline) > 1 && pipelineStageIsOutputFilter(pipeline[len(pipeline)-1]) {
		pipeline = pipeline[:len(pipeline)-1]
	}
	for i := range pipeline {
		pipeline[i] = normalizeShellWords(pipeline[i])
	}
	base := strings.Join(pipeline, " | ")
	if base == "" {
		return ""
	}
	if path, ok := classifyShellRead(base); ok {
		return "read:" + path
	}
	if commandLikelyLongRunningCheck(base) {
		return "check:" + truncateCommandFamily(base, checkLabelTokens)
	}
	return "bash:" + truncateCommandFamily(base, 1)
}

const checkLabelTokens = 4

func truncateCommandFamily(command string, keepTokens int) string {
	fields := strings.Fields(stripLeadingEnvAssignments(command))
	if len(fields) == 0 {
		return ""
	}
	if len(fields) > keepTokens {
		fields = fields[:keepTokens]
	}
	return strings.Join(fields, " ")
}

func stripLeadingEnvAssignments(command string) string {
	fields := strings.Fields(command)
	i := 0
	for i < len(fields) && isEnvAssignment(fields[i]) {
		i++
	}
	return strings.Join(fields[i:], " ")
}

func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	name := tok[:eq]
	for i, r := range name {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func normalizeInputPath(input map[string]any) string {
	for _, key := range []string{"file_path", "path", "target_file"} {
		raw, _ := input[key].(string)
		if path := normalizePath(raw); path != "" {
			return path
		}
	}
	return ""
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func normalizeShellWords(command string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
}

func splitShellPipeline(command string) []string {
	var (
		parts   []string
		current strings.Builder
		singleQ bool
		doubleQ bool
		escaped bool
	)
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			current.WriteRune(r)
			escaped = true
		case r == '\'' && !doubleQ:
			singleQ = !singleQ
			current.WriteRune(r)
		case r == '"' && !singleQ:
			doubleQ = !doubleQ
			current.WriteRune(r)
		case r == '|' && !singleQ && !doubleQ:
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, strings.TrimSpace(current.String()))
	return compactNonEmpty(parts)
}

func pipelineStageIsOutputFilter(stage string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(stage)))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "tail", "head", "grep", "cut", "tr", "wc", "nl":
		return true
	case "sed":
		return len(fields) >= 2 && fields[1] == "-n"
	case "awk":
		return true
	default:
		return false
	}
}

func classifyShellRead(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	switch fields[0] {
	case "cat":
		if path := lastNonFlagToken(fields[1:]); path != "" {
			return normalizePath(path), true
		}
	case "tail", "head":
		if path := lastShellReadPathToken(fields[1:]); path != "" {
			return normalizePath(path), true
		}
	case "sed":
		if len(fields) >= 4 && fields[1] == "-n" {
			if path := normalizePath(fields[len(fields)-1]); path != "" {
				return path, true
			}
		}
	}
	return "", false
}

func lastNonFlagToken(tokens []string) string {
	for i := range slices.Backward(tokens) {
		if strings.HasPrefix(tokens[i], "-") {
			continue
		}
		return tokens[i]
	}
	return ""
}

func lastShellReadPathToken(tokens []string) string {
	for i := range slices.Backward(tokens) {
		token := tokens[i]
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if shellReadFlagConsumesToken(tokens, i) {
			continue
		}
		return token
	}
	return ""
}

func shellReadFlagConsumesToken(tokens []string, index int) bool {
	if index == 0 || !shellTokenLooksNumeric(tokens[index]) {
		return false
	}
	switch tokens[index-1] {
	case "-n", "-c", "--lines", "--bytes":
		return true
	default:
		return false
	}
}

func shellTokenLooksNumeric(token string) bool {
	if token == "" {
		return false
	}
	if token[0] == '+' || token[0] == '-' {
		token = token[1:]
	}
	if token == "" {
		return false
	}
	for _, r := range token {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func hashParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	h := fnv.New64a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func compactNonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func compactUnique(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func toolResultSignalsProgress(name string, input map[string]any) bool {
	switch strings.TrimSpace(name) {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return true
	case "Bash":
		cmd, _ := input["command"].(string)
		return bashCommandLikelyMutatesFiles(cmd)
	default:
		return false
	}
}

func bashCommandLikelyMutatesFiles(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"apply_patch",
		"git apply",
		" patch ",
		"sed -i",
		"perl -i",
		"tee ",
		" >",
		">>",
		"cat <<",
		"python ",
		"python3 ",
		"node ",
		"ruby ",
		"touch ",
		"mkdir ",
		"rm ",
		"mv ",
		"cp ",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func commandLikelyLongRunningCheck(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"cargo build",
		"cargo check",
		"cargo clippy",
		"cargo test",
		"go build",
		"go test",
		"go vet",
		"golangci-lint",
		"jest",
		"make build",
		"make check",
		"make lint",
		"make test",
		"mise run build",
		"mise run check",
		"mise run lint",
		"mise run test",
		"mise run verify",
		"npm ci",
		"npm install",
		"npm run build",
		"npm run check",
		"npm run lint",
		"npm run test",
		"npm run verify",
		"npx playwright test",
		"oxlint",
		"playwright test",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
