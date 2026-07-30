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
	normalizedRoot   *yaml.Node
	data             []byte
	normalizedData   []byte
	warnings         []string
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
	if _, ok := f.authoredNodeAt(path...); ok {
		return true
	}
	_, ok := f.nodeAt(path...)
	return ok
}

func (f *FileConfig) Warnings() []string {
	if f == nil {
		return nil
	}
	return append([]string(nil), f.warnings...)
}

func (f *FileConfig) nodeAt(path ...string) (*yaml.Node, bool) {
	if f == nil {
		return nil, false
	}
	node := f.root
	if f.normalizedRoot != nil {
		node = f.normalizedRoot
	}
	if node == nil {
		return nil, false
	}
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

func (f *FileConfig) authoredNodeAt(path ...string) (*yaml.Node, bool) {
	if f == nil {
		return nil, false
	}
	return yamlNodeAt(f.root, path...)
}

func yamlNodeAt(node *yaml.Node, path ...string) (*yaml.Node, bool) {
	if node == nil {
		return nil, false
	}
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
	validateRoot := &root
	if cfg.schemaVersion >= CurrentSchemaVersion {
		normalized, warnings, err := NormalizeV2Document(&root)
		if err != nil {
			return nil, err
		}
		cfg.normalizedRoot = normalized
		cfg.warnings = warnings
		cfg.normalizedData, err = marshalYAMLDocument(normalized)
		if err != nil {
			return nil, err
		}
		validateRoot = normalized
		cfg.warnings = append(cfg.warnings, legacyFieldAliasWarnings(normalized)...)
	}
	if err := validateKnownConfigKeys(validateRoot, cfg.schemaVersion); err != nil {
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

type fieldAliasSpec struct {
	aliasPath  []string
	legacyPath []string
	fieldPath  []string
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
	{aliasPath: []string{"harness_evolution", "max_report_age"}, legacyPath: []string{"harness_evolution", "max_report_age_hours"}, fieldPath: []string{"HarnessEvolve", "MaxReportAgeHours"}, unit: unitHours, kind: kindFloat},
	{aliasPath: []string{"auto_update", "poll"}, legacyPath: []string{"auto_update", "poll_seconds"}, fieldPath: []string{"AutoUpdate", "PollSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"auto_update", "restart_delay"}, legacyPath: []string{"auto_update", "restart_delay_seconds"}, fieldPath: []string{"AutoUpdate", "RestartDelaySeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"auto_update", "coalesce"}, legacyPath: []string{"auto_update", "coalesce_seconds"}, fieldPath: []string{"AutoUpdate", "CoalesceSeconds"}, unit: unitSeconds, kind: kindInt},
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
	{aliasPath: []string{"github", "polling", "issues", "interval"}, legacyPath: []string{"github", "polling", "issues", "interval_seconds"}, fieldPath: []string{"GitHub", "Polling", "Issues", "IntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "polling", "sybra_prs", "active_interval"}, legacyPath: []string{"github", "polling", "sybra_prs", "active_interval_seconds"}, fieldPath: []string{"GitHub", "Polling", "SybraPRs", "ActiveIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "polling", "sybra_prs", "idle_interval"}, legacyPath: []string{"github", "polling", "sybra_prs", "idle_interval_seconds"}, fieldPath: []string{"GitHub", "Polling", "SybraPRs", "IdleIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "polling", "assigned_prs", "active_interval"}, legacyPath: []string{"github", "polling", "assigned_prs", "active_interval_seconds"}, fieldPath: []string{"GitHub", "Polling", "AssignedPRs", "ActiveIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "polling", "assigned_prs", "idle_interval"}, legacyPath: []string{"github", "polling", "assigned_prs", "idle_interval_seconds"}, fieldPath: []string{"GitHub", "Polling", "AssignedPRs", "IdleIntervalSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "renovate_fast"}, legacyPath: []string{"github", "renovate_fast_seconds"}, fieldPath: []string{"GitHub", "RenovateFastSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"github", "renovate_slow"}, legacyPath: []string{"github", "renovate_slow_seconds"}, fieldPath: []string{"GitHub", "RenovateSlowSeconds"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"experience", "ttl"}, legacyPath: []string{"experience", "ttl_days"}, fieldPath: []string{"Experience", "TTLDays"}, unit: unitDays, kind: kindInt},
	{aliasPath: []string{"agent", "k8s_jobs", "ttl_after_finished"}, legacyPath: []string{"agent", "k8s_jobs", "ttl_seconds_after_finished"}, fieldPath: []string{"Agent", "K8sJobs", "TTL"}, unit: unitSeconds, kind: kindInt},
	{aliasPath: []string{"agent", "k8s_jobs", "failed_ttl_after_finished"}, legacyPath: []string{"agent", "k8s_jobs", "failed_ttl_seconds_after_finished"}, fieldPath: []string{"Agent", "K8sJobs", "FailedTTL"}, unit: unitSeconds, kind: kindInt},
}

var fieldAliasSpecs = []fieldAliasSpec{
	{aliasPath: []string{"github", "webhook", "enabled"}, legacyPath: []string{"webhook", "enabled"}, fieldPath: []string{"GitHub", "Webhook", "Enabled"}},
	{aliasPath: []string{"github", "webhook", "port"}, legacyPath: []string{"webhook", "port"}, fieldPath: []string{"GitHub", "Webhook", "Port"}},
	{aliasPath: []string{"github", "webhook", "task_secret"}, legacyPath: []string{"webhook", "secret"}, fieldPath: []string{"GitHub", "Webhook", "TaskSecret"}},
	{aliasPath: []string{"agent", "post_result_cost_usd"}, legacyPath: []string{"agent", "max_cost_usd"}, fieldPath: []string{"Agent", "MaxCostUSD"}},
	{aliasPath: []string{"agent", "max_assistant_events"}, legacyPath: []string{"agent", "max_turns"}, fieldPath: []string{"Agent", "MaxTurns"}},
	{aliasPath: []string{"agent", "checkpoint_on_assistant_event_ceiling"}, legacyPath: []string{"agent", "checkpoint_on_turn_ceiling"}, fieldPath: []string{"Agent", "CheckpointOnTurnCeiling"}},
	{aliasPath: []string{"agent", "assistant_event_cost_fraction"}, legacyPath: []string{"agent", "turn_cost_fraction"}, fieldPath: []string{"Agent", "TurnCostFraction"}},
	{aliasPath: []string{"agent", "assistant_event_multiplier"}, legacyPath: []string{"agent", "turn_multiplier"}, fieldPath: []string{"Agent", "TurnMultiplier"}},
	// review_rounds_per_hour briefly lived on GitHubConfig (schema v2, one day)
	// before moving to AgentDefaults. Unlike every other entry in this table,
	// aliasPath and legacyPath cross parents (agent vs github) — that's fine
	// for validation and setFieldByPathFromNode (both are keyed off fieldPath,
	// not the parent), but migrateNodeToCanonical only renames a leaf in
	// place, so a raw github.review_rounds_per_hour survives migration under
	// integrations.github rather than moving to execution.agent. Harmless
	// (Resolve applies the same alias on the next parse) but not perfectly
	// canonicalized.
	{aliasPath: []string{"agent", "review_rounds_per_hour"}, legacyPath: []string{"github", "review_rounds_per_hour"}, fieldPath: []string{"Agent", "ReviewRoundsPerHour"}},
}

type aliasIndex struct {
	byParent map[string]map[string]durationAliasSpec
}

type fieldAliasIndex struct {
	byParent map[string]map[string]fieldAliasSpec
}

// removedConfigKeys lists config keys that used to exist but were deleted
// outright (not renamed — see fieldAliasSpecs for renames). A pre-existing
// config.yaml can still carry one of these even after the field is gone from
// the Go struct — a full re-serialize (e.g. Settings save, or config dump)
// writes every field including zero values, so "removed but never actually
// read" keys like agent.mode routinely end up persisted on disk. Validation
// must keep loading such a file instead of failing closed with "unknown
// config key", or every operator upgrading past the removal breaks startup.
var removedConfigKeys = map[string]map[string]bool{
	"agent": {"mode": true},
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

func newFieldAliasIndex(specs []fieldAliasSpec) fieldAliasIndex {
	idx := fieldAliasIndex{byParent: map[string]map[string]fieldAliasSpec{}}
	for _, spec := range specs {
		for _, path := range [][]string{spec.aliasPath, spec.legacyPath} {
			parent := strings.Join(path[:len(path)-1], ".")
			if idx.byParent[parent] == nil {
				idx.byParent[parent] = map[string]fieldAliasSpec{}
			}
			idx.byParent[parent][path[len(path)-1]] = spec
		}
	}
	return idx
}

var schemaV2Aliases = newAliasIndex(durationAliasSpecs)
var schemaV2FieldAliases = newFieldAliasIndex(fieldAliasSpecs)

// DurationAliasPathForLegacy reports the schema-v2 duration alias key path for a
// legacy numeric config path (dotted, e.g. "agent.bash_timeout_seconds" ->
// "agent.bash_timeout"). It returns false when the path has no alias. A sparse
// config patcher uses this to keep alias-form files valid instead of writing a
// legacy key that would conflict with the alias on the next reload.
func DurationAliasPathForLegacy(legacyPath string) (string, bool) {
	for _, spec := range durationAliasSpecs {
		if joinPath(spec.legacyPath) == legacyPath {
			return joinPath(spec.aliasPath), true
		}
	}
	return "", false
}

func fieldAliasPathForLegacy(legacyPath string) (string, bool) {
	for _, spec := range fieldAliasSpecs {
		if joinPath(spec.legacyPath) == legacyPath {
			return joinPath(spec.aliasPath), true
		}
	}
	return "", false
}

func legacyFieldAliasPathsForRuntime(runtimePath string) []string {
	var paths []string
	for _, spec := range fieldAliasSpecs {
		if joinPath(spec.aliasPath) == runtimePath {
			paths = append(paths, joinPath(spec.legacyPath))
		}
	}
	return paths
}

// FormatDurationAliasValue renders a legacy numeric value (int or float) into a
// duration string in the alias's unit for legacyPath, such that reloading
// round-trips back to value. It returns false when legacyPath has no alias or
// value is not a supported numeric type.
func FormatDurationAliasValue(legacyPath string, value any) (string, bool) {
	for _, spec := range durationAliasSpecs {
		if joinPath(spec.legacyPath) != legacyPath {
			continue
		}
		return formatDurationAliasValue(value, spec.unit)
	}
	return "", false
}

func formatDurationAliasValue(value any, unit durationUnit) (string, bool) {
	var num string
	switch v := value.(type) {
	case int:
		num = strconv.Itoa(v)
	case int64:
		num = strconv.FormatInt(v, 10)
	case float64:
		num = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return "", false
	}
	return num + unitSuffix(unit), true
}

func unitSuffix(unit durationUnit) string {
	switch unit {
	case unitSeconds:
		return "s"
	case unitMinutes:
		return "m"
	case unitHours:
		return "h"
	case unitDays:
		return "d"
	default:
		return ""
	}
}

func validateKnownConfigKeys(root *yaml.Node, schemaVersion int) error {
	fieldAliases := schemaV2FieldAliases
	aliases := schemaV2Aliases
	return validateNodeAgainstType(root, reflect.TypeFor[Config](), nil, aliases, fieldAliases)
}

func validateNodeAgainstType(node *yaml.Node, typ reflect.Type, path []string, aliases aliasIndex, fieldAliases fieldAliasIndex) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return validateNodeAgainstType(node.Content[0], typ, path, aliases, fieldAliases)
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		fields := yamlFieldsForType(typ)
		parent := strings.Join(path, ".")
		allowedAliases := aliases.byParent[parent]
		allowedFieldAliases := fieldAliases.byParent[parent]
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			key := keyNode.Value
			if field, ok := fields[key]; ok {
				if err := validateNodeAgainstType(valNode, field, append(path, key), aliases, fieldAliases); err != nil {
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
			if _, ok := allowedFieldAliases[key]; ok {
				continue
			}
			if parent == "" && key == "webhook" {
				if err := validateNodeAgainstType(valNode, reflect.TypeFor[WebhookConfig](), []string{"webhook"}, aliases, fieldAliases); err != nil {
					return err
				}
				continue
			}
			if removedConfigKeys[parent][key] {
				continue
			}
			suggestion := nearestKey(key, knownKeys(fields, allowedAliases, allowedFieldAliases))
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
			if err := validateNodeAgainstType(child, typ.Elem(), append(path, fmt.Sprintf("[%d]", i)), aliases, fieldAliases); err != nil {
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
				if err := validateNodeAgainstType(node.Content[i+1], elem, append(path, key), aliases, fieldAliases); err != nil {
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

func knownKeys(fields map[string]reflect.Type, aliases map[string]durationAliasSpec, fieldAliases map[string]fieldAliasSpec) []string {
	keys := make([]string, 0, len(fields)+len(aliases)+len(fieldAliases))
	for key := range fields {
		keys = append(keys, key)
	}
	for key := range aliases {
		keys = append(keys, key)
	}
	for key := range fieldAliases {
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
	if file == nil {
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

func applyFieldAliases(file *FileConfig, cfg *ResolvedConfig) error {
	if file == nil {
		return nil
	}
	for _, spec := range fieldAliasSpecs {
		legacyNode, hasLegacy := file.nodeAt(spec.legacyPath...)
		if !hasLegacy {
			continue
		}
		if canonicalNode, hasCanonical := file.nodeAt(spec.aliasPath...); hasCanonical {
			same, err := aliasNodesEqualForField(spec.fieldPath, canonicalNode, legacyNode)
			if err != nil {
				return fmt.Errorf("%s: %w", joinPath(spec.aliasPath), err)
			}
			if !same {
				return fmt.Errorf("%s conflicts with legacy compatibility field %q", joinPath(spec.aliasPath), joinPath(spec.legacyPath))
			}
		}
		if err := setFieldByPathFromNode(cfg, spec.fieldPath, legacyNode); err != nil {
			return fmt.Errorf("%s: %w", joinPath(spec.legacyPath), err)
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

func aliasNodesEqualForField(path []string, a, b *yaml.Node) (bool, error) {
	av, err := decodeFieldNodeByPath(path, a)
	if err != nil {
		return false, err
	}
	bv, err := decodeFieldNodeByPath(path, b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(av.Interface(), bv.Interface()), nil
}

func decodeFieldNodeByPath(path []string, node *yaml.Node) (reflect.Value, error) {
	fieldType, err := structFieldTypeByPath(reflect.TypeFor[Config](), path)
	if err != nil {
		return reflect.Value{}, err
	}
	if fieldType.Kind() == reflect.Pointer {
		dst := reflect.New(fieldType.Elem())
		if err := node.Decode(dst.Interface()); err != nil {
			return reflect.Value{}, err
		}
		return dst, nil
	}
	dst := reflect.New(fieldType)
	if err := node.Decode(dst.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return dst.Elem(), nil
}

func structFieldTypeByPath(typ reflect.Type, path []string) (reflect.Type, error) {
	current := typ
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	for _, name := range path {
		if current.Kind() != reflect.Struct {
			return nil, fmt.Errorf("path %q does not resolve to a struct field", strings.Join(path, "."))
		}
		field, ok := current.FieldByName(name)
		if !ok {
			return nil, fmt.Errorf("unknown field %q in path %q", name, strings.Join(path, "."))
		}
		current = field.Type
	}
	return current, nil
}

func legacyFieldAliasWarnings(root *yaml.Node) []string {
	var warnings []string
	for _, spec := range fieldAliasSpecs {
		if _, ok := yamlNodeAt(root, spec.legacyPath...); !ok {
			continue
		}
		warnings = append(warnings,
			fmt.Sprintf("config key %q is deprecated in schema_version %d; migrate to %q",
				joinPath(spec.legacyPath), CurrentSchemaVersion, joinPath(spec.aliasPath)))
	}
	return warnings
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

func setFieldByPathFromNode(cfg *ResolvedConfig, path []string, node *yaml.Node) error {
	val := reflect.ValueOf(cfg).Elem()
	for _, name := range path[:len(path)-1] {
		val = val.FieldByName(name)
	}
	field := val.FieldByName(path[len(path)-1])
	if field.Kind() == reflect.Pointer {
		dst := reflect.New(field.Type().Elem())
		if err := node.Decode(dst.Interface()); err != nil {
			return err
		}
		field.Set(dst)
		return nil
	}
	dst := reflect.New(field.Type())
	if err := node.Decode(dst.Interface()); err != nil {
		return err
	}
	field.Set(dst.Elem())
	return nil
}
