// Command checkgitexec rejects production Go code that constructs Git
// subprocesses outside internal/gitexec. Syntax-aware inspection keeps the
// boundary effective across multiline formatting and os/exec import aliases.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
)

var allowlist = map[string]struct{}{
	"internal/gitexec/gitexec.go": {},
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
			fmt.Fprintf(os.Stderr, "check-git-exec-gate: parse %s: %v\n", path, err)
			failed = true
			continue
		}

		execNames, dotImported := osExecImports(file)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			command, ok := osExecCommand(call.Fun, execNames, dotImported)
			if !ok {
				return true
			}
			arg := 0
			if command == "CommandContext" {
				arg = 1
			}
			if len(call.Args) <= arg || !isGitLiteral(call.Args[arg]) {
				return true
			}
			if !failed {
				fmt.Fprintln(os.Stderr, "ERROR: production Git subprocess spawn outside internal/gitexec.")
				fmt.Fprintln(os.Stderr, "Route the operation through gitexec and keep only domain policy at the caller.")
				fmt.Fprintln(os.Stderr)
			}
			failed = true
			pos := fset.Position(call.Pos())
			fmt.Fprintf(os.Stderr, "  %s:%d:%d: os/exec.%s constructs git\n", path, pos.Line, pos.Column, command)
			return true
		})
	}
	if failed {
		os.Exit(1)
	}
}

func osExecImports(file *ast.File) (map[string]struct{}, bool) {
	names := make(map[string]struct{})
	dotImported := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "os/exec" {
			continue
		}
		if spec.Name == nil {
			names["exec"] = struct{}{}
			continue
		}
		switch spec.Name.Name {
		case ".":
			dotImported = true
		case "_":
		default:
			names[spec.Name.Name] = struct{}{}
		}
	}
	return names, dotImported
}

func osExecCommand(expr ast.Expr, names map[string]struct{}, dotImported bool) (string, bool) {
	switch fun := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if _, ok := names[pkg.Name]; !ok {
			return "", false
		}
		if fun.Sel.Name == "Command" || fun.Sel.Name == "CommandContext" {
			return fun.Sel.Name, true
		}
	case *ast.Ident:
		if dotImported && (fun.Name == "Command" || fun.Name == "CommandContext") {
			return fun.Name, true
		}
	}
	return "", false
}

func isGitLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == "git"
}
