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
	"github.com/Automaat/sybra/internal/sybra"
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
		d, l := exprString(t.Elt)
		return "[]" + d, l
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
	if desc, ok := config.LookupPathDescriptor(yamlPath); ok && desc.Secret {
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
	case reflect.String:
		return fmt.Sprintf("`%q`", normalizePath(v.String()))
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return fmt.Sprintf("`%v`", v.Interface())
	case reflect.Struct, reflect.Slice, reflect.Map:
		return "" // rendered as a nested section / has no scalar default
	default:
		return fmt.Sprintf("`%v`", v.Interface())
	}
}

type section struct {
	title  string
	yaml   string
	doc    string
	fields []renderedField
}

type renderedField struct {
	runtimePath string
	yaml        string
	typeStr     string
	def         string
	doc         string
}

type renderedNamespace struct {
	name          string
	ownershipRule string
	sections      []section
}

// walk builds one section per YAML path reachable from Config, in
// declaration order, skipping types outside the config package (external
// package types like abtest.Config are shown as opaque leaf fields instead
// of being expanded). The same local struct type can appear at several YAML
// paths, so the recursion guard must be path-based rather than type-based.
func walk(pt *pkgTypes, cfg reflect.Value, typeName, yamlPrefix string, goPath []string, seen map[string]bool, out *[]section) {
	seenKey := yamlPrefix
	if seenKey == "" {
		seenKey = "<root>"
	}
	if seen[seenKey] {
		return
	}
	seen[seenKey] = true
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
				runtimePath: yamlPath,
				yaml:        yamlPath,
				typeStr:     f.typeStr,
				def:         "_(see below)_",
				doc:         f.doc,
			})
			continue
		}
		def := defaultValueAt(cfg, append(append([]string{}, goPath...), f.goName), yamlPath)
		sec.fields = append(sec.fields, renderedField{runtimePath: yamlPath, yaml: yamlPath, typeStr: f.typeStr, def: def, doc: f.doc})
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
	b.WriteString("Every key Sybra reads from `~/.sybra/config.yaml`, grouped by the " +
		"schema v2 namespace hierarchy. Defaults shown are the resolved runtime " +
		"values from `config.DefaultConfig()`. Env overrides, aliases, secret " +
		"status, and reload policy come from the shared config descriptor model " +
		"used by the CLI and Settings UI.\n\n" +
		"Regenerate with `go generate ./internal/config/...` after changing a " +
		"struct tag or doc comment; internal/config's " +
		"`TestConfigDocs_InSyncWithSource` fails CI if this file drifts.\n\n")

	b.WriteString("## V2 Namespace Ownership\n\n")
	b.WriteString("| Namespace | Owns | Canonical paths |\n")
	b.WriteString("|---|---|---|\n")
	for _, ns := range config.V2NamespaceDocs() {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` |\n", ns.Name, escapeDoc(ns.OwnershipRule), strings.Join(ns.Paths, "`, `"))
	}
	b.WriteString("\n")

	if schema := schemaSection(sections); schema != nil {
		b.WriteString("## Schema\n\n")
		renderSection(&b, *schema)
	}

	for _, ns := range groupSectionsByNamespace(sections) {
		b.WriteString("## " + titleWord(ns.name) + "\n\n")
		b.WriteString(ns.ownershipRule + "\n\n")
		for _, sec := range ns.sections {
			renderSection(&b, sec)
		}
	}
	return b.String()
}

func escapeDoc(doc string) string {
	doc = strings.ReplaceAll(doc, "\n", " ")
	return strings.ReplaceAll(doc, "|", "\\|")
}

func canonicalDocPath(path string) string {
	if path == "" {
		return ""
	}
	if aliasPath, ok := config.DurationAliasPathForLegacy(path); ok {
		if canonical, ok := config.CanonicalFilePathForLegacy(aliasPath); ok {
			return canonical
		}
		return aliasPath
	}
	if canonical, ok := config.CanonicalFilePathForLegacy(path); ok {
		return canonical
	}
	return path
}

func groupSectionsByNamespace(sections []section) []renderedNamespace {
	docMap := map[string]renderedNamespace{}
	order := []string{}
	for _, ns := range config.V2NamespaceDocs() {
		docMap[ns.Name] = renderedNamespace{name: ns.Name, ownershipRule: ns.OwnershipRule}
		order = append(order, ns.Name)
	}
	addRootDirectFieldSections(docMap, sections)
	for _, sec := range sections {
		if sec.title == "Config" {
			continue
		}
		canonical := canonicalDocPath(sec.yaml)
		namespace := namespaceForPath(canonical)
		if namespace == "" {
			continue
		}
		group := docMap[namespace]
		group.sections = append(group.sections, sec)
		docMap[namespace] = group
	}
	out := make([]renderedNamespace, 0, len(order))
	for _, name := range order {
		group := docMap[name]
		if len(group.sections) == 0 {
			continue
		}
		out = append(out, group)
	}
	return out
}

func addRootDirectFieldSections(docMap map[string]renderedNamespace, sections []section) {
	root := rootConfigSection(sections)
	if root == nil {
		return
	}
	byNamespace := map[string][]renderedField{}
	for _, f := range root.fields {
		if f.yaml == "schema_version" || f.def == "_(see below)_" {
			continue
		}
		namespace := namespaceForPath(canonicalDocPath(f.yaml))
		if namespace == "" {
			continue
		}
		byNamespace[namespace] = append(byNamespace[namespace], f)
	}
	for namespace, fields := range byNamespace {
		group := docMap[namespace]
		group.sections = append([]section{{
			title:  titleWord(namespace) + " Root",
			yaml:   namespace,
			doc:    "Scalar/root keys that live directly under this namespace.",
			fields: fields,
		}}, group.sections...)
		docMap[namespace] = group
	}
}

func namespaceForPath(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func schemaSection(sections []section) *section {
	root := rootConfigSection(sections)
	if root == nil {
		return nil
	}
	out := section{title: "Schema", yaml: "", doc: "Schema negotiation and migration controls."}
	for _, f := range root.fields {
		if f.yaml == "schema_version" {
			out.fields = append(out.fields, f)
		}
	}
	if len(out.fields) == 0 {
		return nil
	}
	return &out
}

func rootConfigSection(sections []section) *section {
	for _, sec := range sections {
		if sec.title == "Config" {
			return &sec
		}
	}
	return nil
}

func renderSection(b *bytes.Buffer, sec section) {
	heading := sec.title
	if canonical := canonicalDocPath(sec.yaml); canonical != "" {
		heading = fmt.Sprintf("%s (`%s`)", sec.title, canonical)
	}
	level := "### "
	if sec.title == "Schema" {
		level = ""
	}
	if level != "" {
		b.WriteString(level + heading + "\n\n")
	}
	if sec.doc != "" {
		b.WriteString(sec.doc + "\n\n")
	}
	if len(sec.fields) == 0 {
		b.WriteString("_No fields._\n\n")
		return
	}
	b.WriteString("| YAML key | Type | Default | Unit | Env override | Legacy aliases | Secret | Reload | Constraints | Description |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range sec.fields {
		meta := docFieldMetadata(f.runtimePath)
		fmt.Fprintf(
			b,
			"| `%s` | `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			canonicalDocPath(f.yaml),
			f.typeStr,
			f.def,
			meta.unit,
			meta.envVars,
			meta.aliases,
			meta.secret,
			meta.reload,
			meta.constraints,
			escapeDoc(f.doc),
		)
	}
	b.WriteString("\n")
}

type fieldMetadata struct {
	unit        string
	envVars     string
	aliases     string
	secret      string
	reload      string
	constraints string
}

func docFieldMetadata(runtimePath string) fieldMetadata {
	desc, ok := config.LookupPathDescriptor(runtimePath)
	if !ok {
		return fieldMetadata{secret: "`false`"}
	}
	meta := fieldMetadata{
		unit:        markdownCell(desc.Unit),
		envVars:     markdownList(desc.EnvVars),
		aliases:     markdownList(desc.LegacyPaths),
		secret:      boolCell(desc.Secret),
		constraints: markdownList(desc.Constraints),
	}
	if reload, ok := sybra.ConfigRegistryMetadataByRuntimePath(desc.RuntimePath); ok {
		meta.reload = markdownCell(string(reload.Policy))
	}
	return meta
}

func markdownList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("`%s`", item))
	}
	return strings.Join(out, ", ")
}

func markdownCell(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("`%s`", s)
}

func boolCell(v bool) string {
	if !v {
		return "`false`"
	}
	return "`true`"
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
