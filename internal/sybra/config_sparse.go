package sybra

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/config"
	"gopkg.in/yaml.v3"
)

type configLeafMutation struct {
	path  string
	value any
}

func patchSettingsRawConfig(raw []byte, before, after *config.Config) ([]byte, error) {
	if after == nil {
		return nil, fmt.Errorf("nil target config")
	}
	defaults := config.DefaultConfig()
	var deletes []string
	var sets []configLeafMutation
	for _, path := range changedConfigLeafPaths(*before, *after) {
		nextValue := configValueAtPath(*after, path)
		if configValuesEqual(nextValue, configValueAtPath(*defaults, path)) {
			deletes = append(deletes, path)
			continue
		}
		sets = append(sets, configLeafMutation{path: path, value: nextValue})
	}
	slices.SortFunc(deletes, func(a, b string) int {
		return strings.Count(b, ".") - strings.Count(a, ".")
	})
	slices.SortFunc(sets, func(a, b configLeafMutation) int {
		if depth := strings.Count(a.path, ".") - strings.Count(b.path, "."); depth != 0 {
			return depth
		}
		return strings.Compare(a.path, b.path)
	})

	current := append([]byte(nil), raw...)
	var err error
	for _, path := range deletes {
		current, err = deleteYAMLPath(current, splitYAMLPath(path))
		if err != nil {
			return nil, err
		}
	}
	for _, mut := range sets {
		current, err = upsertYAMLPath(current, splitYAMLPath(mut.path), mut.value)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func changedConfigLeafPaths(old, next config.Config) []string {
	var out []string
	for _, path := range configLeafPaths {
		if configValuesEqual(configValueAtPath(old, path), configValueAtPath(next, path)) {
			continue
		}
		out = append(out, path)
	}
	return out
}

func splitYAMLPath(path string) []string {
	return strings.Split(path, ".")
}

func deleteYAMLPath(raw []byte, path []string) ([]byte, error) {
	current, removed, err := deleteYAMLPathOnce(raw, path)
	if err != nil || !removed {
		return current, err
	}
	for depth := len(path) - 1; depth > 0; depth-- {
		current, _, err = deleteEmptyMappingPath(current, path[:depth])
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func deleteEmptyMappingPath(raw []byte, path []string) ([]byte, bool, error) {
	root, err := parseYAMLRoot(raw)
	if err != nil {
		return nil, false, err
	}
	entry, ok := findYAMLEntry(root, path)
	if !ok || !yamlValueIsEmptyMapping(entry.value) {
		return raw, false, nil
	}
	return deleteYAMLPathOnce(raw, path)
}

func deleteYAMLPathOnce(raw []byte, path []string) ([]byte, bool, error) {
	root, err := parseYAMLRoot(raw)
	if err != nil {
		return nil, false, err
	}
	entry, ok := findYAMLEntry(root, path)
	if !ok {
		return raw, false, nil
	}
	lines := splitRawLines(raw)
	start, end := yamlEntryBounds(lines, entry)
	if start >= end || start < 0 || end > len(lines) {
		return nil, false, fmt.Errorf("invalid yaml span for %s", strings.Join(path, "."))
	}
	lines = append(lines[:start], lines[end:]...)
	return joinRawLines(lines), true, nil
}

func upsertYAMLPath(raw []byte, path []string, value any) ([]byte, error) {
	root, err := parseYAMLRoot(raw)
	if err != nil {
		return nil, err
	}
	lines := splitRawLines(raw)
	if entry, ok := findYAMLEntry(root, path); ok {
		replacement, err := renderNestedYAMLPath(path[len(path)-1:], value, entry.key.Column-1)
		if err != nil {
			return nil, err
		}
		replLines := splitRawLines(replacement)
		preserveYAMLLineComment(entry, replLines)
		start, end := yamlEntryBounds(lines, entry)
		lines = replaceLineRange(lines, start, end, replLines)
		return joinRawLines(lines), nil
	}
	parent, remaining, hasRoot := deepestExistingYAMLMapping(root, path)
	if !hasRoot || (parent.mapping == nil && len(path) > 0) {
		return renderNestedYAMLPath(path, value, 0)
	}
	if parent.mapping != nil && parent.parentKey == nil && len(parent.mapping.Content) == 0 && len(remaining) == len(path) {
		return renderNestedYAMLPath(path, value, 0)
	}
	replacement, err := renderNestedYAMLPath(remaining, value, yamlChildIndent(parent.parentKey))
	if err != nil {
		return nil, err
	}
	insertAt := yamlMappingInsertLine(lines, parent.mapping, parent.parentKey)
	lines = replaceLineRange(lines, insertAt, insertAt, splitRawLines(replacement))
	return joinRawLines(lines), nil
}

type yamlPathParent struct {
	mapping   *yaml.Node
	parentKey *yaml.Node
}

type yamlPathEntry struct {
	parent    *yaml.Node
	parentKey *yaml.Node
	key       *yaml.Node
	value     *yaml.Node
	keyIndex  int
}

func parseYAMLRoot(raw []byte) (*yaml.Node, error) {
	var root yaml.Node
	if len(bytes.TrimSpace(raw)) == 0 {
		return &root, nil
	}
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func yamlRootMapping(root *yaml.Node) (*yaml.Node, bool) {
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

func deepestExistingYAMLMapping(root *yaml.Node, path []string) (yamlPathParent, []string, bool) {
	current, ok := yamlRootMapping(root)
	if !ok {
		return yamlPathParent{}, path, false
	}
	parent := yamlPathParent{mapping: current}
	if len(path) == 0 {
		return parent, nil, true
	}
	for i, part := range path[:len(path)-1] {
		key, value, _, found := yamlMappingChild(current, part)
		if !found {
			return parent, path[i:], true
		}
		if value.Kind != yaml.MappingNode {
			return parent, path[i:], true
		}
		parent = yamlPathParent{mapping: value, parentKey: key}
		current = value
	}
	return parent, path[len(path)-1:], true
}

func findYAMLEntry(root *yaml.Node, path []string) (yamlPathEntry, bool) {
	current, ok := yamlRootMapping(root)
	if !ok {
		return yamlPathEntry{}, false
	}
	var parentKey *yaml.Node
	for i, part := range path {
		key, value, keyIndex, found := yamlMappingChild(current, part)
		if !found {
			return yamlPathEntry{}, false
		}
		if i == len(path)-1 {
			return yamlPathEntry{
				parent:    current,
				parentKey: parentKey,
				key:       key,
				value:     value,
				keyIndex:  keyIndex,
			}, true
		}
		if value.Kind != yaml.MappingNode {
			return yamlPathEntry{}, false
		}
		parentKey = key
		current = value
	}
	return yamlPathEntry{}, false
}

func yamlMappingChild(mapping *yaml.Node, key string) (keyNode, valueNode *yaml.Node, keyIndex int, ok bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil, 0, false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1], i, true
		}
	}
	return nil, nil, 0, false
}

func yamlEntryBounds(lines []string, entry yamlPathEntry) (start, end int) {
	start = max(entry.key.Line-1, 0)
	if entry.keyIndex+2 < len(entry.parent.Content) {
		return start, max(entry.parent.Content[entry.keyIndex+2].Line-1, start)
	}
	parentIndent := 0
	if entry.parentKey != nil {
		parentIndent = max(entry.parentKey.Column-1, 0)
	}
	return start, yamlBlockEndLine(lines, start, parentIndent)
}

func yamlBlockEndLine(lines []string, start, parentIndent int) int {
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if leadingSpaces(lines[i]) <= parentIndent {
			return i
		}
	}
	return len(lines)
}

func yamlChildIndent(parentKey *yaml.Node) int {
	if parentKey == nil {
		return 0
	}
	return max(parentKey.Column+1, 0)
}

func yamlMappingInsertLine(lines []string, mapping, parentKey *yaml.Node) int {
	if mapping != nil && len(mapping.Content) > 0 {
		last := yamlPathEntry{
			parent:    mapping,
			parentKey: parentKey,
			key:       mapping.Content[len(mapping.Content)-2],
			value:     mapping.Content[len(mapping.Content)-1],
			keyIndex:  len(mapping.Content) - 2,
		}
		_, end := yamlEntryBounds(lines, last)
		return end
	}
	if parentKey != nil {
		return max(parentKey.Line, 0)
	}
	return 0
}

func yamlValueIsEmptyMapping(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind {
	case yaml.MappingNode:
		return len(node.Content) == 0
	case yaml.ScalarNode:
		return strings.TrimSpace(node.Value) == ""
	default:
		return false
	}
}

func renderNestedYAMLPath(path []string, value any, indent int) ([]byte, error) {
	var nested any = value
	for i := len(path) - 1; i >= 0; i-- {
		nested = map[string]any{path[i]: nested}
	}
	data, err := yaml.Marshal(nested)
	if err != nil {
		return nil, err
	}
	lines := splitRawLines(data)
	if indent == 0 {
		return joinRawLines(lines), nil
	}
	prefix := strings.Repeat(" ", indent)
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = prefix + line
	}
	return joinRawLines(lines), nil
}

func splitRawLines(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func joinRawLines(lines []string) []byte {
	if len(lines) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func replaceLineRange(lines []string, start, end int, replacement []string) []string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}
	out := make([]string, 0, len(lines)-(end-start)+len(replacement))
	out = append(out, lines[:start]...)
	out = append(out, replacement...)
	out = append(out, lines[end:]...)
	return out
}

func preserveYAMLLineComment(entry yamlPathEntry, lines []string) {
	if len(lines) == 0 {
		return
	}
	comment := normalizeYAMLLineComment(entry.value.LineComment)
	if comment == "" {
		comment = normalizeYAMLLineComment(entry.key.LineComment)
	}
	if comment == "" {
		return
	}
	lines[0] += " # " + comment
}

func normalizeYAMLLineComment(comment string) string {
	comment = strings.TrimSpace(comment)
	comment = strings.TrimSpace(strings.TrimPrefix(comment, "#"))
	return comment
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
