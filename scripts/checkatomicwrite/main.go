// Command checkatomicwrite rejects hand-rolled atomic writes outside
// internal/fsutil. Syntax-aware inspection keeps the boundary effective across
// multiline formatting, os aliases, and temp variables that are not spelled
// "tmp" — a line regex missed `os.Rename(f.Name(), path)`, the most idiomatic
// spelling of the pattern it exists to catch.
//
// A write is hand-rolled when os.Rename's source flows from os.CreateTemp,
// directly or through a variable. fsutil.AtomicWrite/AtomicWriteMode do the
// whole sequence — write, chmod, fsync the file, rename, fsync the parent
// directory — and every private copy this repo grew skipped that last step.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
)

// Files allowed to rename a temp file into place:
//   - internal/fsutil/fsutil.go: the canonical implementation.
//   - internal/fsutil/fsutil_test.go: exercises it directly.
//   - internal/project/store.go: renames a temp *directory* (a finished bare
//     clone). There is no file content to stage, so the helper does not apply.
//   - internal/confighot/watcher_test.go: simulates an editor's save as the
//     watcher's own input, not a durability claim.
var allowlist = map[string]struct{}{
	"internal/fsutil/fsutil.go":            {},
	"internal/fsutil/fsutil_test.go":       {},
	"internal/project/store.go":            {},
	"internal/confighot/watcher_test.go":   {},
	"scripts/checkatomicwrite/main.go":     {},
	"scripts/checkatomicwrite/testdata.go": {},
}

func main() {
	fset := token.NewFileSet()
	failed := false
	for _, path := range os.Args[1:] {
		path = filepath.ToSlash(path)
		if _, ok := allowlist[path]; ok {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check-atomic-writes: parse %s: %v\n", path, err)
			failed = true
			continue
		}

		osNames := osImports(file)
		if len(osNames) == 0 {
			continue
		}
		temps := tempVars(file, osNames)

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isOSCall(call.Fun, osNames, "Rename") || len(call.Args) == 0 {
				return true
			}
			if !fromTemp(call.Args[0], temps, osNames) {
				return true
			}
			if !failed {
				fmt.Fprintln(os.Stderr, "ERROR: hand-rolled atomic write outside internal/fsutil.")
				fmt.Fprintln(os.Stderr, "Use fsutil.AtomicWrite (inherits the target's mode) or")
				fmt.Fprintln(os.Stderr, "fsutil.AtomicWriteMode (explicit mode). They fsync the parent")
				fmt.Fprintln(os.Stderr, "directory, which every hand-rolled copy here forgot.")
				fmt.Fprintln(os.Stderr)
			}
			failed = true
			pos := fset.Position(call.Pos())
			fmt.Fprintf(os.Stderr, "  %s:%d:%d: os.Rename publishes a temp file\n", path, pos.Line, pos.Column)
			return true
		})
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println("check-atomic-writes: no hand-rolled temp-rename writes outside internal/fsutil")
}

// osImports returns the local names bound to the os package.
func osImports(file *ast.File) map[string]struct{} {
	names := map[string]struct{}{}
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"os"` {
			continue
		}
		switch {
		case imp.Name == nil:
			names["os"] = struct{}{}
		case imp.Name.Name == "_":
		case imp.Name.Name == ".":
			names["."] = struct{}{}
		default:
			names[imp.Name.Name] = struct{}{}
		}
	}
	return names
}

func isOSCall(fun ast.Expr, osNames map[string]struct{}, name string) bool {
	if _, dot := osNames["."]; dot {
		if id, ok := fun.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = osNames[pkg.Name]
	return ok
}

// tempVars collects identifiers assigned from os.CreateTemp, or from the
// Name() of a value that itself came from os.CreateTemp. Names are tracked
// per-file rather than per-scope: this is a conservative gate, and a false
// positive is a comment away from being allowlisted.
func tempVars(file *ast.File, osNames map[string]struct{}) map[string]struct{} {
	temps := map[string]struct{}{}
	for range 2 { // second pass resolves tmp := f.Name() after f is known
		ast.Inspect(file, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assign.Rhs {
				if !isTempSource(rhs, temps, osNames) {
					continue
				}
				// os.CreateTemp returns (*os.File, error): bind the first name.
				if len(assign.Lhs) > i {
					if id, ok := assign.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
						temps[id.Name] = struct{}{}
					}
				}
			}
			return true
		})
	}
	return temps
}

func isTempSource(expr ast.Expr, temps, osNames map[string]struct{}) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	if isOSCall(call.Fun, osNames, "CreateTemp") {
		return true
	}
	return isTempName(call, temps)
}

// isTempName reports whether expr is <temp>.Name().
func isTempName(call *ast.CallExpr, temps map[string]struct{}) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Name" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = temps[id.Name]
	return ok
}

// fromTemp reports whether expr names a staged temp file: a tracked variable,
// a direct <temp>.Name() call, or os.CreateTemp inline.
func fromTemp(expr ast.Expr, temps, osNames map[string]struct{}) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		_, ok := temps[e.Name]
		return ok
	case *ast.CallExpr:
		return isTempSource(e, temps, osNames)
	}
	return false
}
