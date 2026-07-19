package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	LegacySchemaVersion  = 1
	CurrentSchemaVersion = 2
)

// FileConfig preserves what the operator wrote, including whether a key was
// omitted entirely. Resolve consumes it to produce a concrete ResolvedConfig.
type FileConfig struct {
	schemaVersion    int
	hasSchemaVersion bool
	root             *yaml.Node
	data             []byte
}

func (f *FileConfig) SchemaVersion() int {
	if f == nil {
		return LegacySchemaVersion
	}
	return f.schemaVersion
}

func (f *FileConfig) HasSchemaVersion() bool {
	return f != nil && f.hasSchemaVersion
}

func (f *FileConfig) Has(path ...string) bool {
	_, ok := f.nodeAt(path...)
	return ok
}

func (f *FileConfig) nodeAt(path ...string) (*yaml.Node, bool) {
	if f == nil || f.root == nil {
		return nil, false
	}
	node := f.root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			return nil, false
		}
		found := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				node = node.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return node, true
}

func ParseFileConfig(data []byte) (*FileConfig, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	cfg := &FileConfig{root: &root, data: append([]byte(nil), data...)}
	if root.Kind == 0 {
		cfg.schemaVersion = LegacySchemaVersion
		return cfg, nil
	}
	schemaVersion, hasVersion, err := parseSchemaVersion(&root)
	if err != nil {
		return nil, err
	}
	cfg.schemaVersion = schemaVersion
	cfg.hasSchemaVersion = hasVersion
	if err := validateKnownConfigKeys(&root, cfg.schemaVersion); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parseSchemaVersion(root *yaml.Node) (version int, hasVersion bool, err error) {
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind == 0 {
		return LegacySchemaVersion, false, nil
	}
	if node.Kind != yaml.MappingNode {
		return 0, false, fmt.Errorf("config root must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "schema_version" {
			continue
		}
		raw := strings.TrimSpace(node.Content[i+1].Value)
		if raw == "" {
			return 0, true, fmt.Errorf("schema_version must be an integer")
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return 0, true, fmt.Errorf("schema_version must be an integer, got %q", raw)
		}
		switch {
		case v < LegacySchemaVersion:
			return 0, true, fmt.Errorf("schema_version %d is unsupported; supported versions: %d, %d", v, LegacySchemaVersion, CurrentSchemaVersion)
		case v > CurrentSchemaVersion:
			return 0, true, fmt.Errorf("schema_version %d is newer than this Sybra build supports; upgrade Sybra or downgrade config to schema_version %d", v, CurrentSchemaVersion)
		default:
			return v, true, nil
		}
	}
	return LegacySchemaVersion, false, nil
}

type durationAliasSpec struct {
	aliasPath  []string
	legacyPath []string
	fieldPath  []string
	unit       durationUnit
	kind       durationKind
}

type durationUnit int

const (
	unitSeconds durationUnit = iota
	unitMinutes
	unitHours
	unitDays
)

type durationKind int

const (
	kindInt durationKind = iota
	kindFloat
)

var durationAliasSpecs = []durationAliasSpec{
	{aliasPath: []string{"sandbox", "retention"}, legacyPath: []string{"sandbox", "retention_hours"}, fieldPath: []string{"Sandbox", "RetentionHours"}, unit: unitHours, kind: kindInt},
	{aliasPath: []string{"task_snapshot", "interval"}, legacyPath: []string{"task_snapshot", "interval_seconds"}, fieldPath: []string{"TaskSnapshot", "IntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"triage", "poll"}, legacyPath: []string{"triage", "poll_seconds"}, fieldPath: []string{"Triage", "PollSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"monitor", "interval"}, legacyPath: []string{"monitor", "interval_seconds"}, fieldPath: []string{"Monitor", "IntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"monitor", "issue_cooldown"}, legacyPath: []string{"monitor", "issue_cooldown_minutes"}, fieldPath: []string{"Monitor", "IssueCooldownMinutes"}, unit: unitMinutes, kind: kindInt},
	{aliasPath: []string{"monitor", "stuck_human"}, legacyPath: []string{"monitor", "stuck_human_hours"}, fieldPath: []string{"Monitor", "StuckHumanHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"monitor", "lost_agent"}, legacyPath: []string{"monitor", "lost_agent_minutes"}, fieldPath: []string{"Monitor", "LostAgentMinutes"}, unit: unitMinutes, kind: kindInt},
	{aliasPath: []string{"monitor", "pr_gap_grace"}, legacyPath: []string{"monitor", "pr_gap_grace_minutes"}, fieldPath: []string{"Monitor", "PRGapGraceMinutes"}, unit: unitMinutes, kind: kindInt},
	{aliasPath: []string{"orchestrator", "dispatch_interval"}, legacyPath: []string{"orchestrator", "dispatch_interval_seconds"}, fieldPath: []string{"Orchestrator", "DispatchIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"orchestrator", "maintenance_interval"}, legacyPath: []string{"orchestrator", "maintenance_interval_seconds"}, fieldPath: []string{"Orchestrator", "MaintenanceIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"orchestrator", "pressure", "reclaim_cooldown"}, legacyPath: []string{"orchestrator", "pressure", "reclaim_cooldown_seconds"}, fieldPath: []string{"Orchestrator", "Pressure", "ReclaimCooldownSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"orchestrator", "pressure", "sample_interval"}, legacyPath: []string{"orchestrator", "pressure", "sample_interval_seconds"}, fieldPath: []string{"Orchestrator", "Pressure", "SampleIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"watchdog", "run_window"}, legacyPath: []string{"watchdog", "run_window_minutes"}, fieldPath: []string{"Watchdog", "RunWindowMinutes"}, unit: unitMinutes, kind: kindInt},
	{aliasPath: []string{"self_monitor", "interval"}, legacyPath: []string{"self_monitor", "interval_hours"}, fieldPath: []string{"SelfMonitor", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"self_monitor", "issue_cooldown"}, legacyPath: []string{"self_monitor", "issue_cooldown_hours"}, fieldPath: []string{"SelfMonitor", "IssueCooldownHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"self_monitor", "suppression"}, legacyPath: []string{"self_monitor", "suppression_days"}, fieldPath: []string{"SelfMonitor", "SuppressionDays"}, unit: unitDays, kind: kindInt},
	{aliasPath: []string{"learning_digest", "interval"}, legacyPath: []string{"learning_digest", "interval_hours"}, fieldPath: []string{"LearningDigest", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"evaluation", "interval"}, legacyPath: []string{"evaluation", "interval_hours"}, fieldPath: []string{"Evaluation", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"prompt_lab", "interval"}, legacyPath: []string{"prompt_lab", "interval_hours"}, fieldPath: []string{"PromptLab", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"prompt_lab", "lookback"}, legacyPath: []string{"prompt_lab", "lookback_hours"}, fieldPath: []string{"PromptLab", "LookbackHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"prompt_lab", "refile_cooldown"}, legacyPath: []string{"prompt_lab", "refile_cooldown_days"}, fieldPath: []string{"PromptLab", "RefileCooldownDays"}, unit: unitDays, kind: kindFloat},
	{aliasPath: []string{"harness_evolution", "interval"}, legacyPath: []string{"harness_evolution", "interval_hours"}, fieldPath: []string{"HarnessEvolve", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"routing", "interval"}, legacyPath: []string{"routing", "interval_hours"}, fieldPath: []string{"Routing", "IntervalHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"harness_evolution", "lookback"}, legacyPath: []string{"harness_evolution", "lookback_hours"}, fieldPath: []string{"HarnessEvolve", "LookbackHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"auto_update", "poll"}, legacyPath: []string{"auto_update", "poll_seconds"}, fieldPath: []string{"AutoUpdate", "PollSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"auto_update", "restart_delay"}, legacyPath: []string{"auto_update", "restart_delay_seconds"}, fieldPath: []string{"AutoUpdate", "RestartDelaySeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"providers", "health_check", "interval"}, legacyPath: []string{"providers", "health_check", "interval_seconds"}, fieldPath: []string{"Providers", "HealthCheck", "IntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"providers", "claude", "rate_limit_cooldown"}, legacyPath: []string{"providers", "claude", "rate_limit_cooldown_seconds"}, fieldPath: []string{"Providers", "Claude", "RateLimitCooldownSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"providers", "codex", "rate_limit_cooldown"}, legacyPath: []string{"providers", "codex", "rate_limit_cooldown_seconds"}, fieldPath: []string{"Providers", "Codex", "RateLimitCooldownSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"providers", "copilot", "rate_limit_cooldown"}, legacyPath: []string{"providers", "copilot", "rate_limit_cooldown_seconds"}, fieldPath: []string{"Providers", "Copilot", "RateLimitCooldownSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"providers", "opencode", "rate_limit_cooldown"}, legacyPath: []string{"providers", "opencode", "rate_limit_cooldown_seconds"}, fieldPath: []string{"Providers", "OpenCode", "RateLimitCooldownSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"agent", "bash_timeout"}, legacyPath: []string{"agent", "bash_timeout_seconds"}, fieldPath: []string{"Agent", "BashTimeoutSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"agent", "log_retention"}, legacyPath: []string{"agent", "log_retention_days"}, fieldPath: []string{"Agent", "LogRetentionDays"}, unit: unitDays, kind: kindInt},
	{aliasPath: []string{"agent", "log_gzip_after"}, legacyPath: []string{"agent", "log_gzip_after_days"}, fieldPath: []string{"Agent", "LogGzipAfterDays"}, unit: unitDays, kind: kindInt},
	{aliasPath: []string{"github", "reviews_fast"}, legacyPath: []string{"github", "reviews_fast_seconds"}, fieldPath: []string{"GitHub", "ReviewsFastSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "reviews_slow"}, legacyPath: []string{"github", "reviews_slow_seconds"}, fieldPath: []string{"GitHub", "ReviewsSlowSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "issues"}, legacyPath: []string{"github", "issues_seconds"}, fieldPath: []string{"GitHub", "IssuesSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "renovate_fast"}, legacyPath: []string{"github", "renovate_fast_seconds"}, fieldPath: []string{"GitHub", "RenovateFastSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "renovate_slow"}, legacyPath: []string{"github", "renovate_slow_seconds"}, fieldPath: []string{"GitHub", "RenovateSlowSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"experience", "ttl"}, legacyPath: []string{"experience", "ttl_days"}, fieldPath: []string{"Experience", "TTLDays"}, unit: unitDays, kind: kindInt},
	{aliasPath: []string{"agent", "k8s_jobs", "ttl_after_finished"}, legacyPath: []string{"agent", "k8s_jobs", "ttl_seconds_after_finished"}, fieldPath: []string{"Agent", "K8sJobs", "TTL"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"agent", "k8s_jobs", "failed_ttl_after_finished"}, legacyPath: []string{"agent", "k8s_jobs", "failed_ttl_seconds_after_finished"}, fieldPath: []string{"Agent", "K8sJobs", "FailedTTL"}, unit: unitSeconds, kind: kindInt},
}

type aliasIndex struct {
	byParent map[string]map[string]durationAliasSpec
}

func newAliasIndex(specs []durationAliasSpec) aliasIndex {
	idx := aliasIndex{byParent: map[string]map[string]durationAliasSpec{}}
	for _, spec := range specs {
		parent := strings.Join(spec.aliasPath[:len(spec.aliasPath)-1], ".")
		if idx.byParent[parent] == nil {
			idx.byParent[parent] = map[string]durationAliasSpec{}
		}
		idx.byParent[parent][spec.aliasPath[len(spec.aliasPath)-1]] = spec
	}
	return idx
}

var schemaV2Aliases = newAliasIndex(durationAliasSpecs)

func validateKnownConfigKeys(root *yaml.Node, schemaVersion int) error {
	aliases := aliasIndex{}
	if schemaVersion >= CurrentSchemaVersion {
		aliases = schemaV2Aliases
	}
	return validateNodeAgainstType(root, reflect.TypeFor[Config](), nil, aliases)
}

func validateNodeAgainstType(node *yaml.Node, typ reflect.Type, path []string, aliases aliasIndex) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return validateNodeAgainstType(node.Content[0], typ, path, aliases)
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		fields := yamlFieldsForType(typ)
		parent := strings.Join(path, ".")
		allowedAliases := aliases.byParent[parent]
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			key := keyNode.Value
			if field, ok := fields[key]; ok {
				if err := validateNodeAgainstType(valNode, field, append(path, key), aliases); err != nil {
					return err
				}
				continue
			}
			if spec, ok := allowedAliases[key]; ok {
				if err := validateDurationAliasNode(valNode, spec); err != nil {
					return fmt.Errorf("%s: %w", joinPath(append(path, key)), err)
				}
				continue
			}
			suggestion := nearestKey(key, knownKeys(fields, allowedAliases))
			msg := fmt.Sprintf("unknown config key %q", joinPath(append(path, key)))
			if suggestion != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", suggestion)
			}
			return fmt.Errorf("%s", msg)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, child := range node.Content {
			if err := validateNodeAgainstType(child, typ.Elem(), append(path, fmt.Sprintf("[%d]", i)), aliases); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		if elem := typ.Elem(); elem.Kind() == reflect.Struct || elem.Kind() == reflect.Pointer || elem.Kind() == reflect.Slice || elem.Kind() == reflect.Map {
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i].Value
				if err := validateNodeAgainstType(node.Content[i+1], elem, append(path, key), aliases); err != nil {
					return err
				}
			}
		}
	default:
		return nil
	}
	return nil
}

func validateDurationAliasNode(node *yaml.Node, spec durationAliasSpec) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a duration string")
	}
	_, err := convertDurationAliasValue(strings.TrimSpace(node.Value), spec.unit, spec.kind)
	return err
}

func yamlFieldsForType(typ reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for field := range typ.Fields() {
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = field.Type
	}
	return fields
}

func knownKeys(fields map[string]reflect.Type, aliases map[string]durationAliasSpec) []string {
	keys := make([]string, 0, len(fields)+len(aliases))
	for key := range fields {
		keys = append(keys, key)
	}
	for key := range aliases {
		keys = append(keys, key)
	}
	return keys
}

func nearestKey(key string, candidates []string) string {
	best := ""
	bestDist := 3
	for _, candidate := range candidates {
		if d := levenshtein(key, candidate); d < bestDist {
			best = candidate
			bestDist = d
		}
	}
	return best
}

func joinPath(path []string) string {
	if len(path) == 0 {
		return "<root>"
	}
	var b strings.Builder
	for i, part := range path {
		if strings.HasPrefix(part, "[") {
			b.WriteString(part)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr := make([]int, len(b)+1)
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = curr
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func applyDurationAliases(file *FileConfig, cfg *ResolvedConfig) error {
	if file == nil || file.SchemaVersion() < CurrentSchemaVersion {
		return nil
	}
	for _, spec := range durationAliasSpecs {
		aliasNode, hasAlias := file.nodeAt(spec.aliasPath...)
		if !hasAlias {
			continue
		}
		converted, err := convertDurationAliasValue(strings.TrimSpace(aliasNode.Value), spec.unit, spec.kind)
		if err != nil {
			return fmt.Errorf("%s: %w", joinPath(spec.aliasPath), err)
		}
		if legacyNode, hasLegacy := file.nodeAt(spec.legacyPath...); hasLegacy {
			if legacyNode.Kind != yaml.ScalarNode {
				return fmt.Errorf("%s: legacy compatibility field must be scalar", joinPath(spec.legacyPath))
			}
			if strings.TrimSpace(legacyNode.Value) != converted {
				return fmt.Errorf("%s conflicts with legacy compatibility field %q", joinPath(spec.aliasPath), joinPath(spec.legacyPath))
			}
		}
		if err := setStringFieldByPath(cfg, spec.fieldPath, converted); err != nil {
			return fmt.Errorf("%s: %w", joinPath(spec.aliasPath), err)
		}
	}
	return nil
}

func convertDurationAliasValue(raw string, unit durationUnit, kind durationKind) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("must be a duration string")
	}
	d, err := parseFlexibleDuration(raw)
	if err != nil {
		return "", err
	}
	switch kind {
	case kindInt:
		n, ok := durationAsInt(d, unit)
		if !ok {
			return "", fmt.Errorf("duration %q must be a whole %s", raw, unitName(unit))
		}
		return strconv.Itoa(n), nil
	case kindFloat:
		return strconv.FormatFloat(durationAsFloat(d, unit), 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported duration conversion")
	}
}

func parseFlexibleDuration(raw string) (time.Duration, error) {
	if v, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	return d, nil
}

func durationAsInt(d time.Duration, unit durationUnit) (int, bool) {
	f := durationAsFloat(d, unit)
	n := int(f)
	return n, float64(n) == f
}

func durationAsFloat(d time.Duration, unit durationUnit) float64 {
	switch unit {
	case unitSeconds:
		return d.Seconds()
	case unitMinutes:
		return d.Minutes()
	case unitHours:
		return d.Hours()
	case unitDays:
		return d.Hours() / 24
	default:
		return 0
	}
}

func unitName(unit durationUnit) string {
	switch unit {
	case unitSeconds:
		return "second"
	case unitMinutes:
		return "minute"
	case unitHours:
		return "hour"
	case unitDays:
		return "day"
	default:
		return "unit"
	}
}

func setStringFieldByPath(cfg *ResolvedConfig, path []string, raw string) error {
	val := reflect.ValueOf(cfg).Elem()
	for _, name := range path[:len(path)-1] {
		val = val.FieldByName(name)
	}
	field := val.FieldByName(path[len(path)-1])
	switch field.Kind() {
	case reflect.Int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	case reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		field.SetFloat(f)
	default:
		return fmt.Errorf("unsupported destination kind %s", field.Kind())
	}
	return nil
}
