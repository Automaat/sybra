package config

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type V2NamespaceDoc struct {
	Name          string
	OwnershipRule string
	Paths         []string
}

var v2NamespaceDocs = []V2NamespaceDoc{
	{Name: "instance", OwnershipRule: "Machine role, local routing, and operator-scoped UX defaults.", Paths: []string{"instance", "instance.project_types"}},
	{Name: "execution", OwnershipRule: "How Sybra launches and routes agent work across providers and local backends.", Paths: []string{"execution.agent", "execution.providers"}},
	{Name: "workflow", OwnershipRule: "Task-stage policy, planning/testing orchestration, and board-driven automation.", Paths: []string{"workflow.orchestrator", "workflow.testing", "workflow.triage", "workflow.umbrella", "workflow.admission"}},
	{Name: "integrations", OwnershipRule: "External systems Sybra talks to on the operator's behalf.", Paths: []string{"integrations.notification", "integrations.github", "integrations.renovate", "integrations.browser"}},
	{Name: "supervision", OwnershipRule: "Health checks, review escalation, and autonomous oversight loops.", Paths: []string{"supervision.human_review", "supervision.monitor", "supervision.watchdog", "supervision.self_monitor", "supervision.evaluation", "supervision.learning_digest", "supervision.harness_evolution", "supervision.prompt_lab"}},
	{Name: "storage", OwnershipRule: "Filesystem-backed retention and path layout under SYBRA_HOME.", Paths: []string{"storage.attachments", "storage.trash", "storage.sandboxes", "storage.task_snapshot", "storage.paths"}},
	{Name: "observability", OwnershipRule: "Logs, audit, metrics, experimentation, and operator evidence retention.", Paths: []string{"observability.logging", "observability.audit", "observability.metrics", "observability.experience", "observability.intervention", "observability.ab_testing"}},
	{Name: "routing", OwnershipRule: "Adaptive provider-routing policy that tunes experiment weights from observed execution outcomes.", Paths: []string{"routing"}},
	{Name: "server", OwnershipRule: "Local API/server exposure and auth for the running Sybra instance.", Paths: []string{"server"}},
	{Name: "cluster", OwnershipRule: "Cluster/task-trust policy for multi-node execution backends.", Paths: []string{"cluster"}},
	{Name: "auto_update", OwnershipRule: "Deployment self-update behavior for long-running Sybra installs.", Paths: []string{"auto_update"}},
}

func V2NamespaceDocs() []V2NamespaceDoc {
	out := make([]V2NamespaceDoc, len(v2NamespaceDocs))
	copy(out, v2NamespaceDocs)
	for i := range out {
		out[i].Paths = slices.Clone(out[i].Paths)
	}
	return out
}

type topLevelNamespaceRule struct {
	legacyKey   string
	canonical   []string
	deprecated  string
	namespace   string
	directField bool
}

var topLevelNamespaceRules = []topLevelNamespaceRule{
	{legacyKey: "logging", canonical: []string{"observability", "logging"}, deprecated: "observability.logging", namespace: "observability"},
	{legacyKey: "audit", canonical: []string{"observability", "audit"}, deprecated: "observability.audit", namespace: "observability"},
	{legacyKey: "attachments", canonical: []string{"storage", "attachments"}, deprecated: "storage.attachments", namespace: "storage"},
	{legacyKey: "trash", canonical: []string{"storage", "trash"}, deprecated: "storage.trash", namespace: "storage"},
	{legacyKey: "sandbox", canonical: []string{"storage", "sandboxes"}, deprecated: "storage.sandboxes", namespace: "storage"},
	{legacyKey: "task_snapshot", canonical: []string{"storage", "task_snapshot"}, deprecated: "storage.task_snapshot", namespace: "storage"},
	{legacyKey: "agent", canonical: []string{"execution", "agent"}, deprecated: "execution.agent", namespace: "execution"},
	{legacyKey: "providers", canonical: []string{"execution", "providers"}, deprecated: "execution.providers", namespace: "execution"},
	{legacyKey: "testing", canonical: []string{"workflow", "testing"}, deprecated: "workflow.testing", namespace: "workflow"},
	{legacyKey: "orchestrator", canonical: []string{"workflow", "orchestrator"}, deprecated: "workflow.orchestrator", namespace: "workflow"},
	{legacyKey: "triage", canonical: []string{"workflow", "triage"}, deprecated: "workflow.triage", namespace: "workflow"},
	{legacyKey: "umbrella", canonical: []string{"workflow", "umbrella"}, deprecated: "workflow.umbrella", namespace: "workflow"},
	{legacyKey: "admission", canonical: []string{"workflow", "admission"}, deprecated: "workflow.admission", namespace: "workflow"},
	{legacyKey: "notification", canonical: []string{"integrations", "notification"}, deprecated: "integrations.notification", namespace: "integrations"},
	{legacyKey: "github", canonical: []string{"integrations", "github"}, deprecated: "integrations.github", namespace: "integrations"},
	{legacyKey: "webhook", canonical: []string{"integrations", "github", "webhook"}, deprecated: "integrations.github.webhook", namespace: "integrations"},
	{legacyKey: "review_hold", canonical: []string{"integrations", "github", "review_hold"}, deprecated: "integrations.github.review_hold", namespace: "integrations"},
	{legacyKey: "renovate", canonical: []string{"integrations", "renovate"}, deprecated: "integrations.renovate", namespace: "integrations"},
	{legacyKey: "browser", canonical: []string{"integrations", "browser"}, deprecated: "integrations.browser", namespace: "integrations"},
	{legacyKey: "human_review", canonical: []string{"supervision", "human_review"}, deprecated: "supervision.human_review", namespace: "supervision"},
	{legacyKey: "monitor", canonical: []string{"supervision", "monitor"}, deprecated: "supervision.monitor", namespace: "supervision"},
	{legacyKey: "watchdog", canonical: []string{"supervision", "watchdog"}, deprecated: "supervision.watchdog", namespace: "supervision"},
	{legacyKey: "self_monitor", canonical: []string{"supervision", "self_monitor"}, deprecated: "supervision.self_monitor", namespace: "supervision"},
	{legacyKey: "evaluation", canonical: []string{"supervision", "evaluation"}, deprecated: "supervision.evaluation", namespace: "supervision"},
	{legacyKey: "learning_digest", canonical: []string{"supervision", "learning_digest"}, deprecated: "supervision.learning_digest", namespace: "supervision"},
	{legacyKey: "harness_evolution", canonical: []string{"supervision", "harness_evolution"}, deprecated: "supervision.harness_evolution", namespace: "supervision"},
	{legacyKey: "prompt_lab", canonical: []string{"supervision", "prompt_lab"}, deprecated: "supervision.prompt_lab", namespace: "supervision"},
	{legacyKey: "metrics", canonical: []string{"observability", "metrics"}, deprecated: "observability.metrics", namespace: "observability"},
	{legacyKey: "experience", canonical: []string{"observability", "experience"}, deprecated: "observability.experience", namespace: "observability"},
	{legacyKey: "intervention", canonical: []string{"observability", "intervention"}, deprecated: "observability.intervention", namespace: "observability"},
	{legacyKey: "ab_testing", canonical: []string{"observability", "ab_testing"}, deprecated: "observability.ab_testing", namespace: "observability"},
	{legacyKey: "project_types", canonical: []string{"instance", "project_types"}, deprecated: "instance.project_types", namespace: "instance", directField: true},
	{legacyKey: "tasks_dir", canonical: []string{"storage", "paths", "tasks"}, deprecated: "storage.paths.tasks", namespace: "storage", directField: true},
	{legacyKey: "skills_dir", canonical: []string{"storage", "paths", "skills"}, deprecated: "storage.paths.skills", namespace: "storage", directField: true},
	{legacyKey: "repo_dir", canonical: []string{"storage", "paths", "repo"}, deprecated: "storage.paths.repo", namespace: "storage", directField: true},
	{legacyKey: "projects_dir", canonical: []string{"storage", "paths", "projects"}, deprecated: "storage.paths.projects", namespace: "storage", directField: true},
	{legacyKey: "clones_dir", canonical: []string{"storage", "paths", "clones"}, deprecated: "storage.paths.clones", namespace: "storage", directField: true},
	{legacyKey: "worktrees_dir", canonical: []string{"storage", "paths", "worktrees"}, deprecated: "storage.paths.worktrees", namespace: "storage", directField: true},
	{legacyKey: "loop_agents_dir", canonical: []string{"storage", "paths", "loop_agents"}, deprecated: "storage.paths.loop_agents", namespace: "storage", directField: true},
}

type MigrationMove struct {
	From      string `json:"from"`
	To        string `json:"to"`
	ValueFrom string `json:"valueFrom,omitempty"`
	ValueTo   string `json:"valueTo,omitempty"`
}

type MigrationResult struct {
	ToVersion   int             `json:"toVersion"`
	Changed     bool            `json:"changed"`
	MigratedRaw []byte          `json:"-"`
	Moves       []MigrationMove `json:"moves,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
}

func CanonicalFilePathForLegacy(path string) (string, bool) {
	if alias, ok := fieldAliasPathForLegacy(path); ok {
		path = alias
	}
	for _, rule := range topLevelNamespaceRules {
		if path == rule.legacyKey {
			return joinPath(rule.canonical), true
		}
		if suffix, ok := strings.CutPrefix(path, rule.legacyKey+"."); ok {
			mapped := joinPath(append(slices.Clone(rule.canonical), strings.Split(suffix, ".")...))
			if alias, ok := fieldAliasPathForLegacy(mapped); ok {
				return alias, true
			}
			return mapped, true
		}
	}
	return "", false
}

// unknownKeySink decides what a namespace does with a key this build does not
// know. A strict caller gets an error and stops; a diagnostic collects the
// path and skips the key, so the rest of the document still resolves and every
// stale key is reported in one pass rather than one per run.
type unknownKeySink struct {
	lenient   bool
	collected []string
}

func (s *unknownKeySink) reject(path string) error {
	if s == nil || !s.lenient {
		return fmt.Errorf("%w %q", ErrUnknownConfigKey, path)
	}
	s.collected = append(s.collected, path)
	return nil
}

// NormalizeV2Document rewrites a schema v2 document into the flat internal
// shape, failing on any key it does not know.
func NormalizeV2Document(root *yaml.Node) (*yaml.Node, []string, error) {
	doc, warnings, _, err := normalizeV2Document(root, &unknownKeySink{})
	return doc, warnings, err
}

// normalizeV2Document returns the unknown keys it skipped alongside the
// document, so a lenient caller can report them without losing everything
// else in the file.
func normalizeV2Document(root *yaml.Node, sink *unknownKeySink) (doc *yaml.Node, warnings, unknown []string, err error) {
	builder := newFlatConfigBuilder()
	warnings = []string{}
	top, ok := yamlDocumentMapping(root)
	if !ok {
		if root == nil || root.Kind == 0 {
			return root, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("config root must be a mapping")
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		keyNode := top.Content[i]
		valueNode := top.Content[i+1]
		key := keyNode.Value
		if key == "schema_version" {
			builder.setScalarTopLevel("schema_version", strings.TrimSpace(valueNode.Value))
			continue
		}
		if handled, warn, err := normalizeLegacyTopLevel(builder, key, valueNode); handled {
			if warn != "" {
				warnings = append(warnings, warn)
			}
			if err != nil {
				return nil, nil, nil, err
			}
			continue
		}
		switch key {
		case "instance":
			if err := normalizeInstanceNamespace(builder, valueNode, sink); err != nil {
				return nil, nil, nil, err
			}
		case "execution":
			if err := normalizeSimpleNamespaceSink(builder, valueNode, sink, map[string]string{
				"agent":     "agent",
				"providers": "providers",
			}, "execution"); err != nil {
				return nil, nil, nil, err
			}
		case "workflow":
			if err := normalizeSimpleNamespaceSink(builder, valueNode, sink, map[string]string{
				"orchestrator": "orchestrator",
				"testing":      "testing",
				"triage":       "triage",
				"umbrella":     "umbrella",
				"admission":    "admission",
			}, "workflow"); err != nil {
				return nil, nil, nil, err
			}
		case "integrations":
			if err := normalizeIntegrationsNamespace(builder, valueNode, sink); err != nil {
				return nil, nil, nil, err
			}
		case "supervision":
			if err := normalizeSimpleNamespaceSink(builder, valueNode, sink, map[string]string{
				"human_review":      "human_review",
				"monitor":           "monitor",
				"watchdog":          "watchdog",
				"self_monitor":      "self_monitor",
				"evaluation":        "evaluation",
				"learning_digest":   "learning_digest",
				"harness_evolution": "harness_evolution",
				"prompt_lab":        "prompt_lab",
			}, "supervision"); err != nil {
				return nil, nil, nil, err
			}
		case "storage":
			if err := normalizeStorageNamespace(builder, valueNode, sink); err != nil {
				return nil, nil, nil, err
			}
		case "observability":
			if err := normalizeSimpleNamespaceSink(builder, valueNode, sink, map[string]string{
				"logging":      "logging",
				"audit":        "audit",
				"metrics":      "metrics",
				"experience":   "experience",
				"intervention": "intervention",
				"ab_testing":   "ab_testing",
			}, "observability"); err != nil {
				return nil, nil, nil, err
			}
		case "routing", "server", "cluster", "auto_update", "webhook":
			if err := builder.setTopLevel(key, valueNode, key); err != nil {
				return nil, nil, nil, err
			}
		default:
			if err := sink.reject(key); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	return builder.document(), warnings, sink.collected, nil
}

func MigrateRawConfig(raw []byte, toVersion int) (*MigrationResult, error) {
	if toVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("unsupported migration target %d", toVersion)
	}
	fileCfg, err := ParseFileConfig(raw)
	if err != nil {
		return nil, err
	}
	root, ok := yamlDocumentMapping(fileCfg.root)
	if !ok {
		root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	flatRoot, _, err := normalizeFlatRootForMigration(fileCfg)
	if err != nil {
		return nil, err
	}
	canonical := newCanonicalConfigBuilder()
	canonical.setScalarTopLevel("schema_version", strconv.Itoa(CurrentSchemaVersion))
	for i := 0; i+1 < len(flatRoot.Content); i += 2 {
		key := flatRoot.Content[i].Value
		if key == "schema_version" {
			continue
		}
		value := flatRoot.Content[i+1]
		if err := migrateLegacyTopLevelIntoCanonical(canonical, key, value, nil); err != nil {
			return nil, err
		}
	}
	applyLegacyUnboundedReviewLoop(canonical, fileCfg)
	relocateLegacyGitHubReviewRoundsPerHour(canonical)
	preserveLegacyWebhookTaskRoute(canonical, fileCfg)
	canonicalBytes, err := marshalYAMLDocument(canonical.document())
	if err != nil {
		return nil, err
	}
	moves, err := collectMigrationMoves(root)
	if err != nil {
		return nil, err
	}
	changed := !bytes.Equal(normalizeYAMLBytes(raw), normalizeYAMLBytes(canonicalBytes))
	return &MigrationResult{
		ToVersion:   CurrentSchemaVersion,
		Changed:     changed,
		MigratedRaw: canonicalBytes,
		Moves:       moves,
		Warnings:    fileCfg.Warnings(),
	}, nil
}

func preserveLegacyWebhookTaskRoute(canonical *canonicalConfigBuilder, fileCfg *FileConfig) {
	if fileCfg == nil {
		return
	}
	enabledNode, ok := fileCfg.authoredNodeAt("webhook", "enabled")
	if !ok {
		return
	}
	var enabled bool
	if err := enabledNode.Decode(&enabled); err != nil || !enabled {
		return
	}
	integrationsNode, ok := yamlMappingValue(canonical.root, "integrations")
	if !ok {
		return
	}
	githubNode, ok := yamlMappingValue(integrationsNode, "github")
	if !ok {
		return
	}
	webhookNode, ok := yamlMappingValue(githubNode, "webhook")
	if !ok || yamlMappingHasKey(webhookNode, "task_enabled") {
		return
	}
	webhookNode.Content = append(webhookNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "task_enabled"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	)
}

func normalizeYAMLBytes(raw []byte) []byte {
	return bytes.TrimSpace(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
}

func normalizeFlatRootForMigration(fileCfg *FileConfig) (*yaml.Node, []string, error) {
	if fileCfg == nil {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil, nil
	}
	if fileCfg.SchemaVersion() >= CurrentSchemaVersion {
		doc, warnings, err := NormalizeV2Document(fileCfg.root)
		if err != nil {
			return nil, nil, err
		}
		root, ok := yamlDocumentMapping(doc)
		if !ok {
			return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, warnings, nil
		}
		return root, warnings, nil
	}
	root, ok := yamlDocumentMapping(fileCfg.root)
	if !ok {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil, nil
	}
	return root, nil, nil
}

func collectMigrationMoves(root *yaml.Node) ([]MigrationMove, error) {
	flat, _, err := NormalizeV2Document(root)
	if err != nil {
		return nil, err
	}
	flatRoot, ok := yamlDocumentMapping(flat)
	if !ok {
		return nil, nil
	}
	var moves []MigrationMove
	for i := 0; i+1 < len(flatRoot.Content); i += 2 {
		key := flatRoot.Content[i].Value
		if key == "schema_version" {
			continue
		}
		value := flatRoot.Content[i+1]
		if err := collectMovesForNode(&moves, key, value); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(moves, func(a, b MigrationMove) int { return strings.Compare(a.From, b.From) })
	return moves, nil
}

func collectMovesForNode(out *[]MigrationMove, legacyPath string, node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		if isDurationLegacyParent(legacyPath) {
			for i := 0; i+1 < len(node.Content); i += 2 {
				childPath := legacyPath + "." + node.Content[i].Value
				if err := collectMovesForNode(out, childPath, node.Content[i+1]); err != nil {
					return err
				}
			}
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			childPath := legacyPath + "." + node.Content[i].Value
			if err := collectMovesForNode(out, childPath, node.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		to, ok := CanonicalFilePathForLegacy(legacyPath)
		if !ok {
			return nil
		}
		*out = append(*out, MigrationMove{From: legacyPath, To: to})
	default:
		to, ok := canonicalMovePathForLegacy(legacyPath)
		if !ok {
			return nil
		}
		before, after := renderMoveValues(legacyPath, node)
		*out = append(*out, MigrationMove{From: legacyPath, To: to, ValueFrom: before, ValueTo: after})
	}
	return nil
}

func canonicalMovePathForLegacy(legacyPath string) (string, bool) {
	if aliasPath, ok := DurationAliasPathForLegacy(legacyPath); ok {
		if canonical, ok := CanonicalFilePathForLegacy(aliasPath); ok {
			return canonical, true
		}
	}
	if canonical, ok := CanonicalFilePathForLegacy(legacyPath); ok {
		return canonical, true
	}
	if IsSecretYAMLPath(legacyPath) {
		return legacyPath, true
	}
	return "", false
}

func renderMoveValues(legacyPath string, node *yaml.Node) (before, after string) {
	before = redactedScalarForPath(legacyPath, node)
	after = before
	if aliasPath, ok := DurationAliasPathForLegacy(legacyPath); ok {
		if formatted, ok := FormatDurationAliasValue(legacyPath, yamlScalarNumber(node)); ok {
			after = formatted
			legacyPath = aliasPath
		}
	}
	if IsSecretYAMLPath(legacyPath) {
		after = RedactedPlaceholder
	}
	return before, after
}

func yamlScalarNumber(node *yaml.Node) any {
	if node == nil {
		return nil
	}
	if i, err := strconv.Atoi(strings.TrimSpace(node.Value)); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(node.Value), 64); err == nil {
		return f
	}
	return strings.TrimSpace(node.Value)
}

func redactedScalarForPath(path string, node *yaml.Node) string {
	if IsSecretYAMLPath(path) {
		return RedactedPlaceholder
	}
	if node == nil {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func isDurationLegacyParent(path string) bool {
	for _, spec := range durationAliasSpecs {
		parent := strings.Join(spec.legacyPath[:len(spec.legacyPath)-1], ".")
		if path == parent {
			return true
		}
	}
	return false
}

type flatConfigBuilder struct {
	root *yaml.Node
	seen map[string]string
}

func newFlatConfigBuilder() *flatConfigBuilder {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	return &flatConfigBuilder{root: root, seen: map[string]string{}}
}

func (b *flatConfigBuilder) document() *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{b.root}}
}

func (b *flatConfigBuilder) setScalarTopLevel(key, value string) {
	b.root.Content = append(b.root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value},
	)
}

func (b *flatConfigBuilder) setTopLevel(dest string, value *yaml.Node, source string) error {
	if prior, ok := b.seen[dest]; ok {
		return fmt.Errorf("config key %q is ambiguous between %q and %q", dest, prior, source)
	}
	b.seen[dest] = source
	b.root.Content = append(b.root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: dest},
		cloneYAMLNode(value),
	)
	return nil
}

// applyLegacyUnboundedReviewLoop mirrors the pre-#2499 "review_until_clean:
// true implies an uncapped review→fix→review loop" compat shim, now
// expressed through the single review-rate-limit knob
// (execution.agent.review_rounds_per_hour) instead of a second
// allow-unbounded flag: a legacy config that opted into the old uncapped loop
// keeps that behavior by disabling the rate limit, rather than losing it
// silently now that the old keys are gone from the schema. Skipped if the
// legacy file explicitly set either now-dead key, or already carries a rate
// limit of its own under either the current (agent) or briefly-lived legacy
// (github) key.
func applyLegacyUnboundedReviewLoop(canonical *canonicalConfigBuilder, fileCfg *FileConfig) {
	if fileCfg == nil || fileCfg.SchemaVersion() >= CurrentSchemaVersion {
		return
	}
	if fileCfg.Has("agent", "allow_unbounded_review_rounds") || fileCfg.Has("agent", "max_review_rounds") ||
		fileCfg.Has("agent", "review_rounds_per_hour") || fileCfg.Has("github", "review_rounds_per_hour") {
		return
	}
	executionNode, ok := yamlMappingValue(canonical.root, "execution")
	if !ok {
		return
	}
	agentNode, ok := yamlMappingValue(executionNode, "agent")
	if !ok || !yamlMappingBool(agentNode, "review_until_clean") {
		return
	}
	if yamlMappingHasKey(agentNode, "review_rounds_per_hour") {
		return
	}
	agentNode.Content = append(agentNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "review_rounds_per_hour"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "-1"},
	)
}

// relocateLegacyGitHubReviewRoundsPerHour moves review_rounds_per_hour from
// integrations.github to execution.agent in the migrated canonical tree.
// fieldAliasSpecs' generic leaf-rename only fires within a node's existing
// parent, so the field's one-day stint on GitHubConfig would otherwise
// survive migration under its old namespace forever — functionally harmless
// (Resolve applies the same alias on every parse) but never actually
// canonicalized. Skipped if execution.agent already carries an explicit
// value, leaving any real conflict for Resolve's alias check to catch.
func relocateLegacyGitHubReviewRoundsPerHour(canonical *canonicalConfigBuilder) {
	integrationsNode, ok := yamlMappingValue(canonical.root, "integrations")
	if !ok {
		return
	}
	githubNode, ok := yamlMappingValue(integrationsNode, "github")
	if !ok {
		return
	}
	value, ok := yamlMappingValue(githubNode, "review_rounds_per_hour")
	if !ok {
		return
	}
	executionNode, ok := yamlMappingValue(canonical.root, "execution")
	if !ok {
		return
	}
	agentNode, ok := yamlMappingValue(executionNode, "agent")
	if !ok || yamlMappingHasKey(agentNode, "review_rounds_per_hour") {
		return
	}
	yamlMappingRemove(githubNode, "review_rounds_per_hour")
	agentNode.Content = append(agentNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "review_rounds_per_hour"},
		value,
	)
}

type canonicalConfigBuilder struct {
	root *yaml.Node
}

func newCanonicalConfigBuilder() *canonicalConfigBuilder {
	return &canonicalConfigBuilder{root: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
}

func (b *canonicalConfigBuilder) document() *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{b.root}}
}

func (b *canonicalConfigBuilder) setScalarTopLevel(key, value string) {
	b.root.Content = append(b.root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value},
	)
}

func (b *canonicalConfigBuilder) setPath(path []string, value *yaml.Node) error {
	if len(path) == 0 {
		return nil
	}
	current := b.root
	for _, part := range path[:len(path)-1] {
		child, ok := yamlMappingValue(current, part)
		if !ok {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part},
				child,
			)
		}
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("canonical path %q collides with a scalar", joinPath(path))
		}
		current = child
	}
	key := path[len(path)-1]
	if _, exists := yamlMappingValue(current, key); exists {
		return fmt.Errorf("canonical path %q is ambiguous", joinPath(path))
	}
	current.Content = append(current.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		cloneYAMLNode(value),
	)
	return nil
}

func yamlDocumentMapping(root *yaml.Node) (*yaml.Node, bool) {
	if root == nil {
		return nil, false
	}
	node := root
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, false
		}
		node = node.Content[0]
	}
	if node.Kind == 0 {
		return nil, false
	}
	if node.Kind != yaml.MappingNode {
		return nil, false
	}
	return node, true
}

func normalizeLegacyTopLevel(builder *flatConfigBuilder, key string, value *yaml.Node) (handled bool, warning string, err error) {
	for _, rule := range topLevelNamespaceRules {
		if key != rule.legacyKey {
			continue
		}
		handled = true
		warning = fmt.Sprintf("config key %q is deprecated in schema_version %d; migrate to %q", key, CurrentSchemaVersion, rule.deprecated)
		err = builder.setTopLevel(rule.legacyKey, value, key)
		return handled, warning, err
	}
	return false, "", nil
}

func normalizeSimpleNamespaceSink(builder *flatConfigBuilder, node *yaml.Node, sink *unknownKeySink, allowed map[string]string, prefix string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", prefix)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		dest, ok := allowed[key]
		if !ok {
			if err := sink.reject(prefix + "." + key); err != nil {
				return err
			}
			continue
		}
		if err := builder.setTopLevel(dest, node.Content[i+1], prefix+"."+key); err != nil {
			return err
		}
	}
	return nil
}

func normalizeInstanceNamespace(builder *flatConfigBuilder, node *yaml.Node, sink *unknownKeySink) error {
	return normalizeSimpleNamespaceSink(builder, node, sink, map[string]string{"project_types": "project_types"}, "instance")
}

func normalizeIntegrationsNamespace(builder *flatConfigBuilder, node *yaml.Node, sink *unknownKeySink) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("integrations must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch key {
		case "notification", "renovate", "browser":
			if err := builder.setTopLevel(key, value, "integrations."+key); err != nil {
				return err
			}
		case "github":
			if err := normalizeGitHubNamespace(builder, value); err != nil {
				return err
			}
		default:
			if err := sink.reject("integrations." + key); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeGitHubNamespace(builder *flatConfigBuilder, node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("integrations.github must be a mapping")
	}
	var githubContent []*yaml.Node
	var reviewHold *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		if key == "review_hold" {
			reviewHold = cloneYAMLNode(value)
			continue
		}
		githubContent = append(githubContent, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, cloneYAMLNode(value))
	}
	if len(githubContent) > 0 {
		if err := builder.setTopLevel("github", &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: githubContent}, "integrations.github"); err != nil {
			return err
		}
	}
	if reviewHold != nil {
		if err := builder.setTopLevel("review_hold", reviewHold, "integrations.github.review_hold"); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStorageNamespace(builder *flatConfigBuilder, node *yaml.Node, sink *unknownKeySink) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("storage must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch key {
		case "attachments":
			if err := builder.setTopLevel("attachments", value, "storage.attachments"); err != nil {
				return err
			}
		case "trash":
			if err := builder.setTopLevel("trash", value, "storage.trash"); err != nil {
				return err
			}
		case "sandboxes":
			if err := builder.setTopLevel("sandbox", value, "storage.sandboxes"); err != nil {
				return err
			}
		case "task_snapshot":
			if err := builder.setTopLevel("task_snapshot", value, "storage.task_snapshot"); err != nil {
				return err
			}
		case "paths":
			if err := normalizeStoragePathsNamespace(builder, value, sink); err != nil {
				return err
			}
		default:
			if err := sink.reject("storage." + key); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeStoragePathsNamespace(builder *flatConfigBuilder, node *yaml.Node, sink *unknownKeySink) error {
	return normalizeSimpleNamespaceSink(builder, node, sink, map[string]string{
		"tasks":       "tasks_dir",
		"skills":      "skills_dir",
		"repo":        "repo_dir",
		"projects":    "projects_dir",
		"clones":      "clones_dir",
		"worktrees":   "worktrees_dir",
		"loop_agents": "loop_agents_dir",
	}, "storage.paths")
}

func migrateLegacyTopLevelIntoCanonical(builder *canonicalConfigBuilder, key string, value *yaml.Node, parent []string) error {
	for _, rule := range topLevelNamespaceRules {
		if key != rule.legacyKey {
			continue
		}
		transformed, err := migrateNodeToCanonical([]string{key}, value)
		if err != nil {
			return err
		}
		return builder.setPath(rule.canonical, transformed)
	}
	return builder.setPath(append(slices.Clone(parent), key), value)
}

func migrateNodeToCanonical(path []string, node *yaml.Node) (*yaml.Node, error) {
	if node == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	if aliasPath, ok := fieldAliasPathForLegacy(joinPath(path)); ok && node.Kind == yaml.ScalarNode {
		_ = aliasPath
		return cloneYAMLNode(node), nil
	}
	if aliasPath, ok := DurationAliasPathForLegacy(joinPath(path)); ok && node.Kind == yaml.ScalarNode {
		rendered, ok := FormatDurationAliasValue(joinPath(path), yamlScalarNumber(node))
		if !ok {
			return nil, fmt.Errorf("format duration alias for %s", joinPath(path))
		}
		_ = aliasPath
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rendered}, nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for i := 0; i+1 < len(node.Content); i += 2 {
			childKey := node.Content[i].Value
			childPath := append(slices.Clone(path), childKey)
			if aliasPath, ok := fieldAliasPathForLegacy(joinPath(childPath)); ok {
				childKey = lastPathPart(aliasPath)
				childPath = append(slices.Clone(path), childKey)
			}
			if aliasPath, ok := DurationAliasPathForLegacy(joinPath(childPath)); ok {
				rendered, ok := FormatDurationAliasValue(joinPath(childPath), yamlScalarNumber(node.Content[i+1]))
				if !ok {
					return nil, fmt.Errorf("format duration alias for %s", joinPath(childPath))
				}
				out.Content = append(out.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lastPathPart(aliasPath)},
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rendered},
				)
				continue
			}
			child, err := migrateNodeToCanonical(childPath, node.Content[i+1])
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: childKey},
				child,
			)
		}
		return out, nil
	default:
		return cloneYAMLNode(node), nil
	}
}

func lastPathPart(path string) string {
	parts := strings.Split(path, ".")
	return parts[len(parts)-1]
}

func yamlMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], true
		}
	}
	return nil, false
}

func yamlMappingHasKey(mapping *yaml.Node, key string) bool {
	_, ok := yamlMappingValue(mapping, key)
	return ok
}

// yamlMappingRemove deletes key from mapping in place, returning its value
// and whether it was present.
func yamlMappingRemove(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			value := mapping.Content[i+1]
			mapping.Content = slices.Delete(mapping.Content, i, i+2)
			return value, true
		}
	}
	return nil, false
}

func yamlMappingBool(mapping *yaml.Node, key string) bool {
	node, ok := yamlMappingValue(mapping, key)
	if !ok {
		return false
	}
	var v bool
	return node.Decode(&v) == nil && v
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	cp := *node
	if len(node.Content) > 0 {
		cp.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			cp.Content[i] = cloneYAMLNode(child)
		}
	}
	return &cp
}

func marshalYAMLDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TimestampedMigrationBackupName(ts time.Time) string {
	return fmt.Sprintf("config.backup.%s.yaml", ts.UTC().Format("20060102T150405Z"))
}
