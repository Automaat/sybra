package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	apiTSConstRe   = regexp.MustCompile(`^export const ([A-Za-z0-9_]+)\b`)
	apiTSImportRe  = regexp.MustCompile(`^import \* as (\w+) from '[^']*/([a-z0-9]+)\.js'`)
	apiHTTPFuncRe  = regexp.MustCompile(`^export (?:async )?function ([A-Za-z0-9_]+)\b`)
	apiHTTPImpRe   = regexp.MustCompile(`^import type \{ (.*) \} from '([^']*)'`)
	bindingImpRe   = regexp.MustCompile(`^import \* as ([A-Za-z0-9_$]+) from "([^"]+)"`)
	bindingImportP = "@wailsio/runtime"
)

func unresolvableMethods(services []service, bindingDir string) (skip map[string]bool, reasons []string, err error) {
	skip = map[string]bool{}
	for _, svc := range services {
		data, readErr := os.ReadFile(filepath.Join(bindingDir, bindingModuleBase(svc.name)+".ts"))
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return nil, nil, readErr
			}
			for _, m := range svc.methods {
				skip[m] = true
			}
			reasons = append(reasons, fmt.Sprintf("%s.* (no binding %s.ts)", svc.name, bindingModuleBase(svc.name)))
			continue
		}
		src := string(data)
		for _, m := range svc.methods {
			if _, _, ok := parseBindingSig(src, m); !ok {
				skip[m] = true
				reasons = append(reasons, svc.name+"."+m+" (no binding signature)")
			}
		}
	}
	return skip, reasons, nil
}

func fillAPITS(src string, services []service, skip map[string]bool) (out string, added []string, err error) {
	lines := strings.Split(src, "\n")

	existing := map[string]bool{}
	aliasByBase := map[string]string{}
	lastAliasLine := map[string]int{}
	for i, line := range lines {
		if m := apiTSConstRe.FindStringSubmatch(line); m != nil {
			existing[m[1]] = true
		}
		if m := apiTSImportRe.FindStringSubmatch(line); m != nil {
			aliasByBase[m[2]] = m[1]
		}
		if _, rest, ok := strings.Cut(line, " = pick("); ok {
			if alias, _, ok := strings.Cut(rest, "."); ok {
				lastAliasLine[alias] = i
			}
		}
	}

	inserts := map[int][]string{}
	for _, svc := range services {
		alias := aliasByBase[bindingModuleBase(svc.name)]
		if alias == "" {
			continue
		}
		for _, method := range svc.methods {
			if existing[method] || skip[method] {
				continue
			}
			anchor, ok := lastAliasLine[alias]
			if !ok {
				return "", nil, fmt.Errorf("api.ts: no existing block for alias %q to place %q", alias, method)
			}
			line := fmt.Sprintf("export const %s = pick(%s.%s, http.%s)", method, alias, method, method)
			inserts[anchor] = append(inserts[anchor], line)
			added = append(added, method)
		}
	}

	return strings.Join(insertAfter(lines, inserts), "\n"), added, nil
}

func parseAPIHTTPImports(lines []string) (imports map[string]*moduleImport, localByModuleType map[string]string, lastImportIdx int) {
	imports = map[string]*moduleImport{}
	localByModuleType = map[string]string{}
	for i, line := range lines {
		if strings.HasPrefix(line, "import ") {
			lastImportIdx = i
		}
		m := apiHTTPImpRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		module := strings.TrimSuffix(strings.TrimPrefix(m[2], importPrefix), ".js")
		mi := &moduleImport{module: module, lineIndex: i}
		for raw := range strings.SplitSeq(m[1], ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			name, local := raw, raw
			if parts := strings.SplitN(raw, " as ", 2); len(parts) == 2 {
				name, local = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			}
			mi.entries = append(mi.entries, importEntry{name: name, local: local})
			localByModuleType[module+"."+name] = local
		}
		imports[module] = mi
	}
	return imports, localByModuleType, lastImportIdx
}

type importEntry struct {
	name  string
	local string
}

type moduleImport struct {
	module    string
	entries   []importEntry
	lineIndex int
}

func lastAPIHTTPCallLineByService(lines []string, services []service) map[string]int {
	lastCallLine := map[string]int{}
	for i, line := range lines {
		for _, svc := range services {
			if strings.Contains(line, "call('"+svc.name+"'") {
				lastCallLine[svc.name] = i
			}
		}
	}
	return lastCallLine
}

func buildAPIHTTPFunctionInserts(services []service, bindingDir string, skip, existing map[string]bool, lastCallLine map[string]int, addImport func(module, typeName string) string) (funcInserts map[int][]string, added []string, err error) {
	funcInserts = map[int][]string{}
	for _, svc := range services {
		var bindingImports map[string]string
		var bindingSrc string
		for _, method := range svc.methods {
			if existing[method] || skip[method] {
				continue
			}
			if bindingSrc == "" {
				data, err := os.ReadFile(filepath.Join(bindingDir, bindingModuleBase(svc.name)+".ts"))
				if err != nil {
					return nil, nil, err
				}
				bindingSrc = string(data)
				bindingImports = parseBindingImports(bindingSrc)
			}
			params, ret, ok := parseBindingSig(bindingSrc, method)
			if !ok {
				return nil, nil, fmt.Errorf("api-http.ts: no binding signature for %s.%s", svc.name, method)
			}
			anchor, ok := lastCallLine[svc.name]
			if !ok {
				return nil, nil, fmt.Errorf("api-http.ts: no existing block for service %q to place %q", svc.name, method)
			}
			sig, refs := renderParams(params, bindingImports, addImport)
			retType := mapType(ret, bindingImports, addImport)
			line := fmt.Sprintf("export function %s(%s): Promise<%s> { return call('%s', '%s'%s) }",
				method, sig, retType, svc.name, method, refs)
			funcInserts[anchor] = append(funcInserts[anchor], line)
			added = append(added, method)
		}
	}
	return funcInserts, added, nil
}

func fillAPIHTTP(src string, services []service, bindingDir string, skip map[string]bool) (out string, added []string, err error) {
	lines := strings.Split(src, "\n")

	existing := map[string]bool{}
	reservedMethodNames := map[string]bool{}
	for _, svc := range services {
		for _, method := range svc.methods {
			reservedMethodNames[method] = true
		}
	}
	for _, line := range lines {
		if m := apiHTTPFuncRe.FindStringSubmatch(line); m != nil {
			existing[m[1]] = true
		}
	}
	imports, localByModuleType, lastImportIdx := parseAPIHTTPImports(lines)
	usedImportLocals := map[string]bool{}
	for _, mi := range imports {
		for _, entry := range mi.entries {
			usedImportLocals[entry.local] = true
		}
	}

	lastCallLine := lastAPIHTTPCallLineByService(lines, services)

	var addedModules []string
	pending := map[string][]importEntry{}
	uniqueLocal := func(base string) string {
		local := base
		for i := 2; reservedMethodNames[local] || usedImportLocals[local]; i++ {
			local = fmt.Sprintf("%s%d", base, i)
		}
		usedImportLocals[local] = true
		return local
	}
	addImport := func(module, typeName string) string {
		key := module + "." + typeName
		if local, ok := localByModuleType[key]; ok {
			return local
		}
		local := typeName
		if reservedMethodNames[local] || usedImportLocals[local] {
			local = uniqueLocal(typeName + "Data")
		} else {
			usedImportLocals[local] = true
		}
		localByModuleType[key] = local
		if _, seen := pending[module]; !seen {
			addedModules = append(addedModules, module)
		}
		pending[module] = append(pending[module], importEntry{name: typeName, local: local})
		return local
	}

	funcInserts, added, err := buildAPIHTTPFunctionInserts(services, bindingDir, skip, existing, lastCallLine, addImport)
	if err != nil {
		return "", nil, err
	}

	importInserts := map[int][]string{}
	for _, module := range addedModules {
		mi, ok := imports[module]
		if ok {
			mi.entries = append(mi.entries, pending[module]...)
			lines[mi.lineIndex] = renderImportLine(module, mi.entries)
			continue
		}
		importInserts[lastImportIdx] = append(importInserts[lastImportIdx], renderImportLine(module, pending[module]))
	}

	merged := map[int][]string{}
	for idx, v := range funcInserts {
		merged[idx] = append(merged[idx], v...)
	}
	for idx, v := range importInserts {
		merged[idx] = append(merged[idx], v...)
	}
	return strings.Join(insertAfter(lines, merged), "\n"), added, nil
}

func renderImportLine(module string, entries []importEntry) string {
	var parts []string
	for _, e := range entries {
		if e.name == e.local {
			parts = append(parts, e.name)
		} else {
			parts = append(parts, e.name+" as "+e.local)
		}
	}
	return fmt.Sprintf("import type { %s } from '%s%s.js'", strings.Join(parts, ", "), importPrefix, module)
}

func parseBindingImports(src string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(src, "\n") {
		if strings.Contains(line, bindingImportP) {
			continue
		}
		if m := bindingImpRe.FindStringSubmatch(line); m != nil {
			rel := strings.TrimSuffix(m[2], ".js")
			out[m[1]] = path.Clean(path.Join(bindingBase, rel))
		}
	}
	return out
}

func parseBindingSig(src, method string) (params, ret string, ok bool) {
	re := regexp.MustCompile(`(?m)^export function ` + regexp.QuoteMeta(method) + `\((.*)\): \$CancellablePromise<(.*)> \{`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func renderParams(params string, bindingImports map[string]string, addImport func(string, string) string) (sig, refs string) {
	params = strings.TrimSpace(params)
	if params == "" {
		return "", ""
	}
	var sigParts, refParts []string
	for i, raw := range splitTopLevel(params) {
		typ := "unknown"
		if _, after, ok := strings.Cut(raw, ":"); ok {
			typ = strings.TrimSpace(after)
		}
		arg := fmt.Sprintf("arg%d", i+1)
		sigParts = append(sigParts, arg+": "+mapType(typ, bindingImports, addImport))
		refParts = append(refParts, arg)
	}
	return strings.Join(sigParts, ", "), ", " + strings.Join(refParts, ", ")
}

func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := range len(s) {
		switch s[i] {
		case '<', '(', '{', '[':
			depth++
		case '>', ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

func mapType(t string, bindingImports map[string]string, addImport func(string, string) string) string {
	t = strings.TrimSpace(t)
	if inner, ok := strings.CutSuffix(t, ")[]"); ok && strings.HasPrefix(inner, "(") {
		return "Array<" + mapType(inner[1:], bindingImports, addImport) + ">"
	}
	if inner, ok := strings.CutSuffix(t, "[]"); ok {
		return "Array<" + mapType(inner, bindingImports, addImport) + ">"
	}
	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		return mapType(t[1:len(t)-1], bindingImports, addImport)
	}
	if s := stripNull(t); s != t {
		return mapType(s, bindingImports, addImport)
	}
	if strings.HasPrefix(t, "Array<") && strings.HasSuffix(t, ">") {
		return "Array<" + mapType(t[len("Array<"):len(t)-1], bindingImports, addImport) + ">"
	}
	switch t {
	case "string", "number", "boolean", "void", "any", "unknown", "null", "":
		return t
	}
	if strings.Contains(t, ".") {
		i := strings.LastIndex(t, ".")
		alias, name := t[:i], t[i+1:]
		module := bindingImports[alias]
		if module == "" {
			if alias == "$models" {
				module = bindingBase + "/models"
			} else {
				return name
			}
		}
		return addImport(module, name)
	}
	return t
}

func stripNull(t string) string {
	if !strings.Contains(t, "|") {
		return t
	}
	var kept []string
	for part := range strings.SplitSeq(t, "|") {
		part = strings.TrimSpace(part)
		if part == "null" || part == "undefined" || part == "" {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, " | ")
}
