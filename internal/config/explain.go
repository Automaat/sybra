package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type ValueSource string

const (
	ValueSourceDefault   ValueSource = "default"
	ValueSourceFile      ValueSource = "file"
	ValueSourceEnv       ValueSource = "env"
	ValueSourceGenerated ValueSource = "generated"
)

type PathDescriptor struct {
	Path        string   `json:"path"`
	RuntimePath string   `json:"runtimePath"`
	QueryPaths  []string `json:"queryPaths,omitempty"`
	LegacyPaths []string `json:"legacyPaths,omitempty"`
	EnvVars     []string `json:"envVars,omitempty"`
	Secret      bool     `json:"secret"`
	Unit        string   `json:"unit,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
}

type PathValue struct {
	Declared bool        `json:"declared"`
	Present  bool        `json:"present"`
	Redacted bool        `json:"redacted,omitempty"`
	Source   ValueSource `json:"source,omitempty"`
	Path     string      `json:"path,omitempty"`
	Value    any         `json:"value,omitempty"`
}

type PathExplanation struct {
	Descriptor PathDescriptor `json:"descriptor"`
	Default    PathValue      `json:"default"`
	Intent     PathValue      `json:"intent"`
	Effective  PathValue      `json:"effective"`
	Override   *PathValue     `json:"override,omitempty"`
}

type envOverrideSpec struct {
	runtimePath string
	envVar      string
	value       func(Environment) (any, bool)
}

type durationDescriptor struct {
	aliasPath string
	unit      string
}

var (
	pathMetadataOnce        sync.Once
	pathDescriptors         []PathDescriptor
	pathDescriptorByRuntime map[string]PathDescriptor
	pathRuntimeByQuery      map[string]string
	durationByRuntime       map[string]durationDescriptor
)

var envOverrideSpecs = []envOverrideSpec{
	{
		runtimePath: "logging.level",
		envVar:      "SYBRA_LOG_LEVEL",
		value: func(env Environment) (any, bool) {
			return env.LogLevel, strings.TrimSpace(env.LogLevel) != ""
		},
	},
	{
		runtimePath: "logging.dir",
		envVar:      "SYBRA_LOG_DIR",
		value: func(env Environment) (any, bool) {
			return env.LogDir, strings.TrimSpace(env.LogDir) != ""
		},
	},
	{
		runtimePath: "tasks_dir",
		envVar:      "SYBRA_TASKS_DIR",
		value: func(env Environment) (any, bool) {
			return env.TasksDir, strings.TrimSpace(env.TasksDir) != ""
		},
	},
	{
		runtimePath: "server.auth_token",
		envVar:      "SYBRA_AUTH_TOKEN",
		value: func(env Environment) (any, bool) {
			return env.AuthToken, strings.TrimSpace(env.AuthToken) != ""
		},
	},
	{
		runtimePath: "server.allowed_origins",
		envVar:      "SYBRA_ALLOWED_ORIGINS",
		value: func(env Environment) (any, bool) {
			if len(env.AllowedOrigins) == 0 {
				return nil, false
			}
			return append([]string(nil), env.AllowedOrigins...), true
		},
	},
	{
		runtimePath: "github.webhook.task_secret",
		envVar:      "SYBRA_WEBHOOK_SECRET",
		value: func(env Environment) (any, bool) {
			return env.WebhookSecret, strings.TrimSpace(env.WebhookSecret) != ""
		},
	},
	{
		runtimePath: "github.webhook.secret",
		envVar:      "SYBRA_GITHUB_WEBHOOK_SECRET",
		value: func(env Environment) (any, bool) {
			return env.GitHubWebhookSecret, strings.TrimSpace(env.GitHubWebhookSecret) != ""
		},
	},
}

var descriptorConstraints = map[string][]string{
	"schema_version": {"supported values: 1, 2"},
}

var errNilYAMLNode = errors.New("nil yaml node")

func YAMLLeafPaths() []string {
	initPathMetadata()
	out := make([]string, len(pathDescriptors))
	for i := range pathDescriptors {
		out[i] = pathDescriptors[i].RuntimePath
	}
	return out
}

func PathDescriptors() []PathDescriptor {
	initPathMetadata()
	return slices.Clone(pathDescriptors)
}

func LookupPathDescriptor(path string) (PathDescriptor, bool) {
	initPathMetadata()
	runtime, ok := pathRuntimeByQuery[path]
	if !ok {
		return PathDescriptor{}, false
	}
	desc, ok := pathDescriptorByRuntime[runtime]
	return desc, ok
}

func NormalizeRuntimeYAMLPath(path string) (string, bool) {
	initPathMetadata()
	runtime, ok := pathRuntimeByQuery[path]
	return runtime, ok
}

func ExplainPath(path string, file *FileConfig, env Environment, resolved *Config) (PathExplanation, error) {
	desc, ok := LookupPathDescriptor(path)
	if !ok {
		return PathExplanation{}, unknownConfigPathError(path)
	}
	explanations := ExplainAll(file, env, resolved)
	for i := range explanations {
		if explanations[i].Descriptor.RuntimePath == desc.RuntimePath {
			return explanations[i], nil
		}
	}
	return PathExplanation{}, unknownConfigPathError(path)
}

func ExplainAll(file *FileConfig, env Environment, resolved *Config) []PathExplanation {
	initPathMetadata()
	defaults := DefaultConfig()
	if resolved == nil {
		resolved = defaults
	}

	out := make([]PathExplanation, 0, len(pathDescriptors))
	for i := range pathDescriptors {
		desc := pathDescriptors[i]
		defaultVal := valueAtRuntimePath(*defaults, desc.RuntimePath)
		effectiveVal := valueAtRuntimePath(*resolved, desc.RuntimePath)
		intent := intentValueForDescriptor(file, desc)
		override := envOverrideValueForDescriptor(env, desc)

		effectiveSource := ValueSourceDefault
		switch {
		case override != nil:
			effectiveSource = ValueSourceEnv
		case intent.Declared:
			effectiveSource = ValueSourceFile
		case generatedValueForDescriptor(desc, effectiveVal):
			effectiveSource = ValueSourceGenerated
		}

		effectivePath := desc.Path
		if override != nil {
			effectivePath = override.Path
		}

		out = append(out, PathExplanation{
			Descriptor: desc,
			Default:    buildPathValue(desc, defaultVal, ValueSourceDefault, desc.Path, true),
			Intent:     intent,
			Effective:  buildPathValue(desc, effectiveVal, effectiveSource, effectivePath, true),
			Override:   override,
		})
	}
	return out
}

func initPathMetadata() {
	pathMetadataOnce.Do(func() {
		leafPaths := collectLeafPaths(reflect.TypeFor[Config](), nil, nil)
		slices.Sort(leafPaths)

		durationByRuntime = map[string]durationDescriptor{}
		for _, spec := range durationAliasSpecs {
			durationByRuntime[joinPath(spec.legacyPath)] = durationDescriptor{
				aliasPath: joinPath(spec.aliasPath),
				unit:      unitWord(spec.unit),
			}
		}

		pathDescriptorByRuntime = make(map[string]PathDescriptor, len(leafPaths))
		pathRuntimeByQuery = map[string]string{}
		pathDescriptors = make([]PathDescriptor, 0, len(leafPaths))
		for _, runtimePath := range leafPaths {
			desc := buildPathDescriptor(runtimePath)
			pathDescriptors = append(pathDescriptors, desc)
			pathDescriptorByRuntime[runtimePath] = desc
			for _, query := range desc.QueryPaths {
				pathRuntimeByQuery[query] = runtimePath
			}
		}
	})
}

func collectLeafPaths(typ reflect.Type, prefix, out []string) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return append(out, joinPath(prefix))
	}
	for field := range typ.Fields() {
		name := yamlTagName(field)
		if name == "" || name == "-" {
			continue
		}
		path := append(slices.Clone(prefix), name)
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		switch fieldType.Kind() {
		case reflect.Struct:
			out = collectLeafPaths(fieldType, path, out)
		default:
			out = append(out, joinPath(path))
		}
	}
	return out
}

func buildPathDescriptor(runtimePath string) PathDescriptor {
	publicPath := canonicalPublicPath(runtimePath)
	legacyPaths := []string{}
	if runtimePath != publicPath {
		legacyPaths = append(legacyPaths, runtimePath)
	}
	legacyPaths = append(legacyPaths, legacyFieldAliasPathsForRuntime(runtimePath)...)
	if alias, ok := durationAliasPathForRuntime(runtimePath); ok && alias != publicPath && alias != runtimePath {
		legacyPaths = append(legacyPaths, alias)
	}
	queryPaths := []string{publicPath}
	queryPaths = append(queryPaths, legacyPaths...)
	if canonicalRuntime, ok := CanonicalFilePathForLegacy(runtimePath); ok && canonicalRuntime != publicPath && canonicalRuntime != runtimePath {
		queryPaths = append(queryPaths, canonicalRuntime)
	}
	queryPaths = dedupeStrings(queryPaths)

	return PathDescriptor{
		Path:        publicPath,
		RuntimePath: runtimePath,
		QueryPaths:  queryPaths,
		LegacyPaths: legacyPaths,
		EnvVars:     envVarsForRuntimePath(runtimePath),
		Secret:      IsSecretYAMLPath(runtimePath),
		Unit:        unitForRuntimePath(runtimePath),
		Constraints: slices.Clone(descriptorConstraints[runtimePath]),
	}
}

func canonicalPublicPath(runtimePath string) string {
	if aliasPath, ok := durationAliasPathForRuntime(runtimePath); ok {
		if canonical, ok := CanonicalFilePathForLegacy(aliasPath); ok {
			return canonical
		}
		return aliasPath
	}
	if canonical, ok := CanonicalFilePathForLegacy(runtimePath); ok {
		return canonical
	}
	return runtimePath
}

func durationAliasPathForRuntime(runtimePath string) (string, bool) {
	desc, ok := durationByRuntime[runtimePath]
	if !ok {
		return "", false
	}
	return desc.aliasPath, true
}

func unitForRuntimePath(runtimePath string) string {
	desc, ok := durationByRuntime[runtimePath]
	if !ok {
		return ""
	}
	return desc.unit
}

func unitWord(unit durationUnit) string {
	switch unit {
	case unitSeconds:
		return "seconds"
	case unitMinutes:
		return "minutes"
	case unitHours:
		return "hours"
	case unitDays:
		return "days"
	default:
		return ""
	}
}

func envVarsForRuntimePath(runtimePath string) []string {
	var out []string
	for _, spec := range envOverrideSpecs {
		if spec.runtimePath == runtimePath {
			out = append(out, spec.envVar)
		}
	}
	return out
}

func intentValueForDescriptor(file *FileConfig, desc PathDescriptor) PathValue {
	if file == nil {
		return PathValue{}
	}
	for _, authoredPath := range desc.QueryPaths {
		node, ok := file.authoredNodeAt(strings.Split(authoredPath, ".")...)
		if !ok {
			continue
		}
		value, err := nodeValue(node)
		if err != nil {
			continue
		}
		return buildPathValue(desc, value, ValueSourceFile, authoredPath, true)
	}
	return PathValue{}
}

func envOverrideValueForDescriptor(env Environment, desc PathDescriptor) *PathValue {
	for _, spec := range envOverrideSpecs {
		if spec.runtimePath != desc.RuntimePath {
			continue
		}
		value, ok := spec.value(env)
		if !ok {
			continue
		}
		override := buildPathValue(desc, value, ValueSourceEnv, spec.envVar, true)
		return &override
	}
	return nil
}

func buildPathValue(desc PathDescriptor, value any, source ValueSource, path string, declared bool) PathValue {
	present := valuePresent(value)
	if desc.Secret && present {
		return PathValue{
			Declared: declared,
			Present:  true,
			Redacted: true,
			Source:   source,
			Path:     path,
			Value:    RedactedPlaceholder,
		}
	}
	return PathValue{
		Declared: declared,
		Present:  present,
		Source:   source,
		Path:     path,
		Value:    value,
	}
}

func valuePresent(value any) bool {
	if value == nil {
		return false
	}
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() > 0
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0
	case reflect.Struct:
		return !v.IsZero()
	default:
		return !v.IsZero()
	}
}

func generatedValueForDescriptor(desc PathDescriptor, value any) bool {
	return desc.RuntimePath == "server.auth_token" && valuePresent(value)
}

func valueAtRuntimePath(cfg Config, path string) any {
	v := reflect.ValueOf(cfg)
	return fieldByRuntimePath(v, path).Interface()
}

func fieldByRuntimePath(v reflect.Value, path string) reflect.Value {
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
			if yamlTagName(sf) != part {
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

func unknownConfigPathError(path string) error {
	initPathMetadata()
	suggestion := nearestKey(path, sortedConfigQueryPaths())
	msg := fmt.Sprintf("unknown config path %q", path)
	if suggestion != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return fmt.Errorf("%s", msg)
}

func sortedConfigQueryPaths() []string {
	keys := make([]string, 0, len(pathRuntimeByQuery))
	for key := range pathRuntimeByQuery {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func dedupeStrings(items []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
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

func nodeValue(node *yaml.Node) (any, error) {
	if node == nil {
		return nil, errNilYAMLNode
	}
	var out any
	if err := node.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
