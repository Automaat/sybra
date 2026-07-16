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
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ""
	}
	pipeline := splitShellPipeline(cmd)
	for len(pipeline) > 1 && pipelineStageIsOutputFilter(pipeline[len(pipeline)-1]) {
		pipeline = pipeline[:len(pipeline)-1]
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
const redactedCommandValue = "[redacted]"

func truncateCommandFamily(command string, keepTokens int) string {
	fields := redactSecretBearingShellTokens(shellFields(stripLeadingEnvAssignments(command)))
	if len(fields) == 0 {
		return ""
	}
	if len(fields) > keepTokens {
		fields = fields[:keepTokens]
	}
	return strings.Join(fields, " ")
}

func stripLeadingEnvAssignments(command string) string {
	return joinShellFields(stripLeadingEnvAssignmentTokens(shellFields(command)))
}

func stripLeadingEnvAssignmentTokens(fields []string) []string {
	i := 0
	for i < len(fields) && isEnvAssignment(fields[i]) {
		i++
	}
	return append([]string(nil), fields[i:]...)
}

func redactSecretBearingShellTokens(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	out := append([]string(nil), fields...)
	for i := range out {
		name, hasInlineValue, ok := parseSecretBearingFlag(out[i])
		if !ok {
			continue
		}
		if hasInlineValue {
			out[i] = name + "=" + redactedCommandValue
			continue
		}
		if i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			out[i+1] = redactedCommandValue
		}
	}
	return out
}

func parseSecretBearingFlag(token string) (name string, hasInlineValue, ok bool) {
	if !strings.HasPrefix(token, "-") {
		return "", false, false
	}
	flag := strings.TrimLeft(token, "-")
	if flag == "" {
		return "", false, false
	}
	name, _, hasInlineValue = strings.Cut(flag, "=")
	if !flagNameLooksSecret(name) {
		return "", false, false
	}
	return token[:len(token)-len(flag)] + name, hasInlineValue, true
}

func flagNameLooksSecret(name string) bool {
	normalized := strings.NewReplacer("_", "-", ".", "-", ":", "-").Replace(strings.ToLower(name))
	parts := strings.FieldsFunc(normalized, func(r rune) bool { return r == '-' })
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		switch part {
		case "auth", "authorization", "bearer", "cookie", "credential", "credentials", "creds", "key", "passwd", "password", "secret", "session", "token":
			return true
		}
	}
	return false
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

func shellFields(command string) []string {
	var (
		fields  []string
		current strings.Builder
		singleQ bool
		doubleQ bool
		escaped bool
	)
	flush := func() {
		if current.Len() == 0 {
			return
		}
		fields = append(fields, current.String())
		current.Reset()
	}
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && !singleQ:
			escaped = true
		case r == '\'' && !doubleQ:
			singleQ = !singleQ
		case r == '"' && !singleQ:
			doubleQ = !doubleQ
		case (r == ' ' || r == '\t' || r == '\n') && !singleQ && !doubleQ:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if escaped {
		current.WriteByte('\\')
	}
	flush()
	return fields
}

func joinShellFields(fields []string) string {
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.IndexFunc(field, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n'
		}) < 0 {
			quoted = append(quoted, field)
			continue
		}
		escaped := strings.ReplaceAll(field, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		quoted = append(quoted, `"`+escaped+`"`)
	}
	return strings.Join(quoted, " ")
}

func pipelineStageIsOutputFilter(stage string) bool {
	fields := shellFields(strings.ToLower(strings.TrimSpace(stage)))
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
	fields := shellFields(command)
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
