// Command gen-config-docs regenerates docs/CONFIG.md from the Go struct
// tags and doc comments in internal/config. Run via `go generate ./...`
// (the directive lives in internal/config/config.go).
//
// The generator parses internal/config's source with go/ast to pull yaml
// tags and field doc comments, and uses reflection over
// config.DefaultConfig() to fill in the default value shown for each field.
// It is not a build-time codegen step (nothing imports its output); it only
// keeps the checked-in reference doc from drifting, enforced by
// internal/config's TestConfigDocs_InSyncWithSource drift guard.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/config"
)

// findModuleRoot walks up from start looking for go.mod.
func findModuleRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found starting from %s", start)
		}
		dir = parent
	}
}

// fieldInfo is one struct field as seen in the Go source, before any
// runtime default is attached.
type fieldInfo struct {
	goName  string
	yaml    string
	typeStr string
	doc     string
	// localType is the local (package config) struct type name this field's
	// type resolves to, if any — used to recurse into nested sections.
	localType string
	isPointer bool
}

// pkgTypes indexes every struct type declared in internal/config by name,
// plus each struct's top-level doc comment.
type pkgTypes struct {
	structs map[string]*ast.StructType
	docs    map[string]string
}

func loadPkgTypes(dir string) (*pkgTypes, error) {
	fset := token.NewFileSet()
	pt := &pkgTypes{structs: map[string]*ast.StructType{}, docs: map[string]string{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				pt.structs[ts.Name.Name] = st
				doc := gd.Doc
				if ts.Doc != nil {
					doc = ts.Doc
				}
				if doc != nil {
					pt.docs[ts.Name.Name] = strings.TrimSpace(doc.Text())
				}
			}
		}
	}
	return pt, nil
}

// exprString renders a field type expression, and returns the bare local
// struct type name (if any) for recursion.
func exprString(e ast.Expr) (display, localType string) {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name, t.Name
	case *ast.StarExpr:
		d, l := exprString(t.X)
		return "*" + d, l
	case *ast.ArrayType:
		d, _ := exprString(t.Elt)
		return "[]" + d, ""
	case *ast.MapType:
		k, _ := exprString(t.Key)
		v, _ := exprString(t.Value)
		return "map[" + k + "]" + v, ""
	case *ast.SelectorExpr:
		pkg := ""
		if id, ok := t.X.(*ast.Ident); ok {
			pkg = id.Name
		}
		return pkg + "." + t.Sel.Name, ""
	default:
		return "?", ""
	}
}

func fieldTag(f *ast.Field) (yamlName string, skip bool) {
	if f.Tag == nil {
		return "", false
	}
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return "", false
	}
	tag := reflect.StructTag(raw)
	y := tag.Get("yaml")
	if y == "" {
		return "", false
	}
	parts := strings.Split(y, ",")
	if parts[0] == "-" {
		return "", true
	}
	return parts[0], false
}

func fieldDoc(f *ast.Field) string {
	if f.Doc != nil {
		return strings.TrimSpace(f.Doc.Text())
	}
	if f.Comment != nil {
		return strings.TrimSpace(f.Comment.Text())
	}
	return ""
}

func structFields(st *ast.StructType) []fieldInfo {
	var out []fieldInfo
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			continue // embedded field, none in this package's config structs
		}
		yamlName, skip := fieldTag(f)
		if skip || yamlName == "" {
			continue
		}
		disp, local := exprString(f.Type)
		_, isPtr := f.Type.(*ast.StarExpr)
		out = append(out, fieldInfo{
			goName:    f.Names[0].Name,
			yaml:      yamlName,
			typeStr:   disp,
			doc:       fieldDoc(f),
			localType: local,
			isPointer: isPtr,
		})
	}
	return out
}

// redactedYAMLPaths are dot-separated yaml key paths (relative to Config)
// whose default-value cell is never rendered, even though they hold no
// secret in the zero-value DefaultConfig() — kept in sync by hand since the
// generator has no way to infer "this yaml key holds a credential" from
// static types alone.
var redactedYAMLPaths = map[string]bool{
	"todoist.api_token": true,
}

// homeDir is config.HomeDir() at generation time — substituted back out of
// path-shaped defaults so the doc (and its CI drift check) is stable across
// machines and users instead of embedding whoever last ran the generator.
var homeDir = config.HomeDir()

func normalizePath(s string) string {
	if homeDir != "" && strings.HasPrefix(s, homeDir) {
		return "~/.sybra" + strings.TrimPrefix(s, homeDir)
	}
	return s
}

func defaultValueAt(v reflect.Value, goPath []string, yamlPath string) string {
	if redactedYAMLPaths[yamlPath] {
		return "`[redacted]`"
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "_(nil)_"
		}
		v = v.Elem()
	}
	for _, name := range goPath {
		if v.Kind() != reflect.Struct {
			return ""
		}
		v = v.FieldByName(name)
		if !v.IsValid() {
			return ""
		}
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return "_(nil)_"
			}
			v = v.Elem()
		}
	}
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Struct, reflect.Slice, reflect.Map:
		return "" // rendered as a nested section / has no scalar default
	default:
		zero := reflect.Zero(v.Type()).Interface()
		val := v.Interface()
		if reflect.DeepEqual(zero, val) {
			return ""
		}
		if s, ok := val.(string); ok {
			val = normalizePath(s)
		}
		return fmt.Sprintf("`%v`", val)
	}
}

type section struct {
	title  string
	yaml   string
	doc    string
	fields []renderedField
}

type renderedField struct {
	yaml    string
	typeStr string
	def     string
	doc     string
}

// walk builds one section per struct type reachable from Config, in
// declaration order, skipping types outside the config package (external
// package types like abtest.Config are shown as opaque leaf fields instead
// of being expanded).
func walk(pt *pkgTypes, cfg reflect.Value, typeName, yamlPrefix string, goPath []string, seen map[string]bool, out *[]section) {
	if seen[typeName] {
		return
	}
	seen[typeName] = true
	st, ok := pt.structs[typeName]
	if !ok {
		return
	}
	sec := section{title: typeName, yaml: yamlPrefix, doc: pt.docs[typeName]}
	var nested []fieldInfo
	for _, f := range structFields(st) {
		yamlPath := f.yaml
		if yamlPrefix != "" {
			yamlPath = yamlPrefix + "." + f.yaml
		}
		if _, isLocal := pt.structs[f.localType]; isLocal {
			nested = append(nested, f)
			sec.fields = append(sec.fields, renderedField{
				yaml:    yamlPath,
				typeStr: f.typeStr,
				def:     "_(see below)_",
				doc:     f.doc,
			})
			continue
		}
		def := defaultValueAt(cfg, append(append([]string{}, goPath...), f.goName), yamlPath)
		sec.fields = append(sec.fields, renderedField{yaml: yamlPath, typeStr: f.typeStr, def: def, doc: f.doc})
	}
	*out = append(*out, sec)
	for _, f := range nested {
		yamlPath := f.yaml
		if yamlPrefix != "" {
			yamlPath = yamlPrefix + "." + f.yaml
		}
		walk(pt, cfg, f.localType, yamlPath, append(append([]string{}, goPath...), f.goName), seen, out)
	}
}

func render(sections []section) string {
	var b bytes.Buffer
	b.WriteString("<!-- Code generated by cmd/gen-config-docs from internal/config. DO NOT EDIT. -->\n\n")
	b.WriteString("# Sybra Configuration Reference\n\n")
	b.WriteString("Every key Sybra reads from `~/.sybra/config.yaml`, grouped by top-level " +
		"section. Defaults shown are `config.DefaultConfig()`'s values; an empty " +
		"Default cell means the Go zero value (unset). Env var overrides applied " +
		"in `config.Load` (e.g. `SYBRA_LOG_LEVEL`, `SYBRA_TASKS_DIR`, " +
		"`SYBRA_TODOIST_TOKEN`) are not shown here — see internal/config/config_defaults.go.\n\n" +
		"Regenerate with `go generate ./internal/config/...` after changing a " +
		"struct tag or doc comment; internal/config's " +
		"`TestConfigDocs_InSyncWithSource` fails CI if this file drifts.\n\n")

	for _, sec := range sections {
		heading := sec.title
		if sec.yaml != "" {
			heading = fmt.Sprintf("%s (`%s`)", sec.title, sec.yaml)
		}
		b.WriteString("## " + heading + "\n\n")
		if sec.doc != "" {
			b.WriteString(sec.doc + "\n\n")
		}
		if len(sec.fields) == 0 {
			b.WriteString("_No fields._\n\n")
			continue
		}
		b.WriteString("| YAML key | Type | Default | Description |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, f := range sec.fields {
			doc := strings.ReplaceAll(f.doc, "\n", " ")
			doc = strings.ReplaceAll(doc, "|", "\\|")
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", f.yaml, f.typeStr, f.def, doc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func main() {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-config-docs:", err)
		os.Exit(1)
	}
	root, err := findModuleRoot(wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-config-docs:", err)
		os.Exit(1)
	}

	pt, err := loadPkgTypes(filepath.Join(root, "internal", "config"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-config-docs:", err)
		os.Exit(1)
	}

	cfg := reflect.ValueOf(*config.DefaultConfig())

	var sections []section
	seen := map[string]bool{}
	walk(pt, cfg, "Config", "", nil, seen, &sections)

	dest := filepath.Join(root, "docs", "CONFIG.md")
	if err := os.WriteFile(dest, []byte(render(sections)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-config-docs: write:", err)
		os.Exit(1)
	}
	fmt.Printf("gen-config-docs: wrote %s (%d sections)\n", dest, len(sections))
}
