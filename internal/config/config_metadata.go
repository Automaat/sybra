package config

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

const RedactedPlaceholder = "[redacted]"

var (
	secretYAMLPathsOnce sync.Once
	secretYAMLPaths     []string
	secretYAMLPathSet   map[string]struct{}
)

// SecretYAMLPaths reports every config leaf tagged as secret.
func SecretYAMLPaths() []string {
	secretYAMLPathsOnce.Do(initSecretMetadata)
	return slices.Clone(secretYAMLPaths)
}

// IsSecretYAMLPath reports whether a dot-separated yaml path is tagged secret.
func IsSecretYAMLPath(path string) bool {
	secretYAMLPathsOnce.Do(initSecretMetadata)
	_, ok := secretYAMLPathSet[path]
	return ok
}

// RedactedCopy returns cfg with every tagged secret field replaced by the
// shared redaction placeholder.
func RedactedCopy(cfg *Config) Config {
	if cfg == nil {
		return Config{}
	}
	out := *cfg
	redactStruct(reflect.ValueOf(&out).Elem())
	return out
}

func initSecretMetadata() {
	pathSet := map[string]struct{}{}
	collectSecretYAMLPaths(pathSet, reflect.TypeFor[Config](), nil)
	secretYAMLPathSet = pathSet
	secretYAMLPaths = make([]string, 0, len(pathSet))
	for path := range pathSet {
		secretYAMLPaths = append(secretYAMLPaths, path)
	}
	slices.Sort(secretYAMLPaths)
}

func collectSecretYAMLPaths(pathSet map[string]struct{}, typ reflect.Type, prefix []string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for sf := range typ.Fields() {
		name := yamlTagName(sf)
		if name == "" || name == "-" {
			continue
		}
		path := append(slices.Clone(prefix), name)
		if sf.Tag.Get("secret") == "true" {
			pathSet[strings.Join(path, ".")] = struct{}{}
		}
		fieldType := sf.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			collectSecretYAMLPaths(pathSet, fieldType, path)
		}
	}
}

func redactStruct(v reflect.Value) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		fv := v.Field(i)
		if sf.Tag.Get("secret") == "true" {
			redactField(fv)
			continue
		}
		fieldType := sf.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			redactStruct(fv)
		}
	}
}

func redactField(v reflect.Value) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.String() != "" {
			v.SetString(RedactedPlaceholder)
		}
	default:
		v.SetZero()
	}
}

func yamlTagName(sf reflect.StructField) string {
	tag := sf.Tag.Get("yaml")
	if tag == "" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}
