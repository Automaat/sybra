package sybra

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/config"
	"gopkg.in/yaml.v3"
)

type configReloadPolicy string

const (
	configPolicyHot       configReloadPolicy = "hot"
	configPolicyRestart   configReloadPolicy = "restart"
	configPolicyImmutable configReloadPolicy = "immutable"
)

type configVisibility string

const (
	configVisibilityUI     configVisibility = "ui"
	configVisibilityRaw    configVisibility = "raw"
	configVisibilitySecret configVisibility = "secret"
)

type configApplyGroup string

const (
	configApplyNone         configApplyGroup = ""
	configApplyAgentRuntime configApplyGroup = "agent-runtime"
	configApplyGuardrails   configApplyGroup = "guardrails"
	configApplyNotification configApplyGroup = "notification"
	configApplyLogLevel     configApplyGroup = "log-level"
)

type configRegistryEntry struct {
	Path       string
	Policy     configReloadPolicy
	Visibility configVisibility
	ApplyGroup configApplyGroup
}

var configRegistry = []configRegistryEntry{
	{Path: "schema_version", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "logging.level", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyLogLevel},
	{Path: "logging.max_size_mb", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "logging.max_files", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "logging.dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "audit", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "attachments", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "trash", Policy: configPolicyHot, Visibility: configVisibilityRaw},
	{Path: "sandbox", Policy: configPolicyHot, Visibility: configVisibilityRaw},
	{Path: "task_snapshot", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "agent", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "testing", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "notification", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyNotification},
	{Path: "orchestrator", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "renovate", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "github", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "umbrella", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "triage", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "human_review", Policy: configPolicyHot, Visibility: configVisibilityRaw},
	{Path: "review_hold", Policy: configPolicyHot, Visibility: configVisibilityRaw},
	{Path: "monitor", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "watchdog", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "self_monitor", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "evaluation", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "learning_digest", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "harness_evolution", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "prompt_lab", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "experience", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "ab_testing", Policy: configPolicyHot, Visibility: configVisibilityRaw},
	{Path: "providers.health_check", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "providers.claude", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "providers.codex", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "providers.copilot", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "providers.opencode", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "providers.limits", Policy: configPolicyHot, Visibility: configVisibilityUI, ApplyGroup: configApplyAgentRuntime},
	{Path: "providers.auto_failover", Policy: configPolicyHot, Visibility: configVisibilityUI},
	{Path: "metrics", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "server", Policy: configPolicyRestart, Visibility: configVisibilitySecret},
	{Path: "webhook", Policy: configPolicyRestart, Visibility: configVisibilitySecret},
	{Path: "cluster", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "auto_update", Policy: configPolicyRestart, Visibility: configVisibilityRaw},
	{Path: "browser", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "project_types", Policy: configPolicyRestart, Visibility: configVisibilityUI},
	{Path: "tasks_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "skills_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "repo_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "projects_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "clones_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "worktrees_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
	{Path: "loop_agents_dir", Policy: configPolicyImmutable, Visibility: configVisibilityRaw},
}

type ConfigMutationResult struct {
	Applied         []string        `json:"applied"`
	RestartRequired []string        `json:"restartRequired"`
	Unchanged       []string        `json:"unchanged"`
	Rejected        []string        `json:"rejected"`
	Recovery        *ConfigRecovery `json:"recovery,omitempty"`
}

type ConfigRecovery struct {
	RestoredLastKnownGood bool   `json:"restoredLastKnownGood"`
	Message               string `json:"message"`
}

type configMutationError struct {
	msg    string
	result ConfigMutationResult
	cause  error
}

func (e *configMutationError) Error() string {
	if e == nil {
		return ""
	}
	if e.msg != "" {
		return e.msg
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return "config mutation failed"
}

func (e *configMutationError) Unwrap() error { return e.cause }

func configMutationErrorf(result ConfigMutationResult, format string, a ...any) error {
	return &configMutationError{msg: fmt.Sprintf(format, a...), result: result}
}

func cloneConfig(src *config.Config) *config.Config {
	if src == nil {
		return nil
	}
	cp := *src
	cp.ProjectTypes = slices.Clone(src.ProjectTypes)
	cp.Server.AllowedOrigins = slices.Clone(src.Server.AllowedOrigins)
	cp.Agent.RoleEffort = cloneStringMap(src.Agent.RoleEffort)
	cp.Agent.PlaywrightMCP.ExtraArgs = slices.Clone(src.Agent.PlaywrightMCP.ExtraArgs)
	cp.Agent.K8sJobs.Command = slices.Clone(src.Agent.K8sJobs.Command)
	cp.Agent.K8sJobs.Env = slices.Clone(src.Agent.K8sJobs.Env)
	cp.Agent.K8sJobs.SecretEnv = slices.Clone(src.Agent.K8sJobs.SecretEnv)
	cp.Agent.K8sJobs.Volumes = slices.Clone(src.Agent.K8sJobs.Volumes)
	cp.ABTesting.Experiments = slices.Clone(src.ABTesting.Experiments)
	if src.ABTesting.BuiltinVersion != nil {
		v := *src.ABTesting.BuiltinVersion
		cp.ABTesting.BuiltinVersion = &v
	}
	return &cp
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	maps.Copy(dst, src)
	return dst
}

func configRegistryCoveragePaths() []string {
	return append([]string(nil), configLeafPaths...)
}

func diffConfig(old, next config.Config) ConfigMutationResult {
	var result ConfigMutationResult
	for _, entry := range configRegistry {
		if configValuesEqual(configValueAtPath(old, entry.Path), configValueAtPath(next, entry.Path)) {
			result.Unchanged = append(result.Unchanged, entry.Path)
			continue
		}
		switch entry.Policy {
		case configPolicyHot:
			result.Applied = append(result.Applied, entry.Path)
		case configPolicyRestart:
			result.RestartRequired = append(result.RestartRequired, entry.Path)
		case configPolicyImmutable:
			result.Rejected = append(result.Rejected, entry.Path)
		}
	}
	return result
}

func configValuesEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	ay, aerr := yaml.Marshal(a)
	by, berr := yaml.Marshal(b)
	if aerr == nil && berr == nil {
		if bytes.Equal(ay, by) {
			return true
		}
		var an, bn any
		if yaml.Unmarshal(ay, &an) == nil && yaml.Unmarshal(by, &bn) == nil {
			return reflect.DeepEqual(an, bn)
		}
	}
	return false
}

func configApplyGroups(paths []string) []configApplyGroup {
	seen := map[configApplyGroup]bool{}
	var groups []configApplyGroup
	for _, path := range paths {
		for _, entry := range configRegistry {
			if entry.Path != path || entry.ApplyGroup == configApplyNone || seen[entry.ApplyGroup] {
				continue
			}
			seen[entry.ApplyGroup] = true
			groups = append(groups, entry.ApplyGroup)
		}
	}
	return groups
}

func configValueAtPath(cfg config.Config, path string) any {
	v := reflect.ValueOf(cfg)
	return fieldByYAMLPath(v, path).Interface()
}

func fieldByYAMLPath(v reflect.Value, path string) reflect.Value {
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	for part := range strings.SplitSeq(path, ".") {
		if v.Kind() != reflect.Struct {
			panic("config path does not resolve to a struct: " + path)
		}
		found := false
		t := v.Type()
		for sf := range t.Fields() {
			if yamlFieldName(sf) != part {
				continue
			}
			v = v.FieldByIndex(sf.Index)
			found = true
			break
		}
		if !found {
			panic("unknown config path: " + path)
		}
	}
	return v
}

func yamlFieldName(sf reflect.StructField) string {
	tag := sf.Tag.Get("yaml")
	if tag == "" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

var configLeafPaths = collectConfigLeafPaths()

func collectConfigLeafPaths() []string {
	var out []string
	walkConfigType(reflect.TypeFor[config.Config](), nil, &out)
	slices.Sort(out)
	return out
}

func walkConfigType(typ reflect.Type, path []string, out *[]string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		*out = append(*out, strings.Join(path, "."))
		return
	}
	for sf := range typ.Fields() {
		name := yamlFieldName(sf)
		if name == "" || name == "-" {
			continue
		}
		fieldType := sf.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		switch fieldType.Kind() {
		case reflect.Struct:
			if fieldType.PkgPath() == "" && fieldType.Name() == "" {
				*out = append(*out, strings.Join(append(path, name), "."))
				continue
			}
			walkConfigType(fieldType, append(path, name), out)
		case reflect.Map, reflect.Slice, reflect.Array:
			*out = append(*out, strings.Join(append(path, name), "."))
		default:
			*out = append(*out, strings.Join(append(path, name), "."))
		}
	}
}

func coveredByRegistry(path string) bool {
	for _, entry := range configRegistry {
		if path == entry.Path || strings.HasPrefix(path, entry.Path+".") {
			return true
		}
	}
	return false
}
