package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Phase 1 has no workspace removal. Every potentially destructive call must
// explain why it cannot touch a workspace, even in docs or a nested module.
func TestRemovalsAreAllowListed(t *testing.T) {
	const list = "testdata/removals.txt"
	data, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	allowed := make(map[string]int)
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) != 4 || fields[0] == "" || fields[2] == "" || strings.TrimSpace(fields[3]) == "" {
			t.Fatalf("%s:%d: expected path<TAB>function<TAB>call<TAB>non-empty reason", list, i+1)
		}
		key := strings.Join(fields[:3], "\t")
		if previous, ok := allowed[key]; ok {
			t.Fatalf("%s:%d: duplicate of line %d", list, i+1, previous)
		}
		allowed[key] = i + 1
	}

	seen := make(map[string]bool)
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, rel, source, 0)
		if err != nil {
			return err
		}
		osName := ""
		for _, imp := range file.Imports {
			if removalString(imp.Path) == "os" {
				osName = "os"
				if imp.Name != nil {
					osName = imp.Name.Name
				}
			}
		}
		for _, decl := range file.Decls {
			function := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = fn.Name.Name
				if fn.Recv != nil {
					receiver := fn.Recv.List[0].Type
					for {
						switch typ := receiver.(type) {
						case *ast.StarExpr:
							receiver = typ.X
						case *ast.IndexExpr:
							receiver = typ.X
						case *ast.IndexListExpr:
							receiver = typ.X
						default:
							function = receiver.(*ast.Ident).Name + "." + function
							goto named
						}
					}
				}
			}
		named:
			ast.Inspect(decl, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, operation := range removalOperations(call, osName) {
					key := rel + "\t" + function + "\t" + operation
					seen[key] = true
					if _, ok := allowed[key]; !ok {
						t.Errorf("%s: removal is not allow-listed: %s", fset.Position(call.Pos()), key)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, line := range allowed {
		if !seen[key] {
			t.Errorf("%s:%d: stale allow-list entry: %s", list, line, key)
		}
	}
}

func removalString(expr ast.Expr) string {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		value, _ := strconv.Unquote(lit.Value)
		return value
	}
	return ""
}

func removalOperations(call *ast.CallExpr, osName string) []string {
	var operations []string
	name := ""
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && osName != "" && pkg.Name == osName {
			name = fun.Sel.Name
		}
	case *ast.Ident:
		if osName == "." {
			name = fun.Name
		}
	}
	if name == "Remove" || name == "RemoveAll" {
		operations = append(operations, "os."+name)
	}
	// Deliberately inspect only direct string literal arguments in this call.
	// This covers exec.Command/CommandContext and wrappers taking "git", and
	// helpers whose name says they run git, such as gitCommand(ctx, "reset").
	var args []string
	if callee := strings.ToLower(calleeName(call)); strings.Contains(callee, "git") {
		args = append(args, "git")
	}
	for _, arg := range call.Args {
		if value := removalString(arg); value != "" {
			args = append(args, value)
		}
	}
	for i, arg := range args {
		if arg != "git" {
			continue
		}
		for j := i + 1; j < len(args); j++ {
			switch args[j] {
			case "reset", "clean":
				operations = append(operations, "git "+args[j])
			case "checkout":
				for _, flag := range args[j+1:] {
					if flag == "-f" || flag == "--force" {
						operations = append(operations, "git checkout -f")
						break
					}
				}
			case "stash", "worktree":
				if j+1 < len(args) && (args[j] == "stash" && args[j+1] == "drop" || args[j] == "worktree" && args[j+1] == "remove") {
					operations = append(operations, "git "+args[j]+" "+args[j+1])
				}
			}
		}
	}
	return operations
}

// calleeName is the name of the function or method a call calls.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}
