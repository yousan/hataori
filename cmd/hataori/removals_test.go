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

// Workspace removal lives in internal/workspace/remove.go only, behind the
// proof of design section 4.6. Every other potentially destructive call must
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
	packages := make(map[string][]*ast.File)
	paths := make(map[*ast.File]string)
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
		key := filepath.Dir(rel) + ":" + file.Name.Name
		packages[key] = append(packages[key], file)
		paths[file] = rel
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range packages {
		gitCalls, slices := removalGitCalls(files)
		for _, file := range files {
			rel := paths[file]
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
					for _, operation := range removalOperations(call, osName, gitCalls[call], slices) {
						key := rel + "\t" + function + "\t" + operation
						seen[key] = true
						if _, ok := allowed[key]; !ok {
							t.Errorf("%s: removal is not allow-listed: %s", fset.Position(call.Pos()), key)
						}
					}
					return true
				})
			}
		}
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

func removalOperations(call *ast.CallExpr, osName string, gitCall bool, slices map[*ast.Object][]string) []string {
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
	// Preserve the original conservative name/literal checks as well as
	// calls discovered through same-package wrapper bodies.
	var args []string
	if callee := strings.ToLower(calleeName(call)); gitCall || strings.Contains(callee, "git") {
		args = append(args, "git")
	}
	for i, arg := range call.Args {
		if call.Ellipsis.IsValid() && i == len(call.Args)-1 {
			args = append(args, removalSliceStrings(arg, slices)...)
		}
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

// removalGitCalls follows calls within one package. Each closure owns its own
// calls: merely defining a git-running closure does not make its parent a wrapper.
func removalGitCalls(files []*ast.File) (map[*ast.CallExpr]bool, map[*ast.Object][]string) {
	functions := make(map[string]ast.Node)
	bodies := make(map[ast.Node]*ast.BlockStmt)
	execNames := make(map[*ast.File]string)
	for _, file := range files {
		for _, imp := range file.Imports {
			if removalString(imp.Path) == "os/exec" {
				name := "exec"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				execNames[file] = name
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch fn := node.(type) {
			case *ast.FuncDecl:
				if fn.Recv == nil {
					functions[fn.Name.Name] = fn
				}
				if fn.Body != nil {
					bodies[fn] = fn.Body
				}
			case *ast.FuncLit:
				bodies[fn] = fn.Body
			}
			return true
		})
	}
	resolve := func(expr ast.Expr) ast.Node {
		switch fun := expr.(type) {
		case *ast.FuncLit:
			return fun
		case *ast.Ident:
			if fun.Obj != nil {
				switch decl := fun.Obj.Decl.(type) {
				case *ast.FuncDecl:
					return decl
				case *ast.AssignStmt:
					for i, lhs := range decl.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Obj == fun.Obj && i < len(decl.Rhs) {
							if lit, ok := decl.Rhs[i].(*ast.FuncLit); ok {
								return lit
							}
						}
					}
				case *ast.ValueSpec:
					for i, id := range decl.Names {
						if id.Obj == fun.Obj && i < len(decl.Values) {
							if lit, ok := decl.Values[i].(*ast.FuncLit); ok {
								return lit
							}
						}
					}
				}
				return nil
			}
			return functions[fun.Name]
		}
		return nil
	}
	calls := make(map[ast.Node][]*ast.CallExpr)
	slices := make(map[*ast.Object][]string)
	for fn, body := range bodies {
		ast.Inspect(body, func(node ast.Node) bool {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
			switch node := node.(type) {
			case *ast.CallExpr:
				calls[fn] = append(calls[fn], node)
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && i < len(node.Rhs) {
						slices[id.Obj] = append(slices[id.Obj], removalSliceStrings(node.Rhs[i], nil)...)
					}
				}
			case *ast.ValueSpec:
				for i, id := range node.Names {
					if i < len(node.Values) {
						slices[id.Obj] = append(slices[id.Obj], removalSliceStrings(node.Values[i], nil)...)
					}
				}
			}
			return true
		})
	}
	gitCalls := make(map[*ast.CallExpr]bool)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call)
			isExec := false
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				isExec = ok && execNames[file] != "" && pkg.Name == execNames[file]
			case *ast.Ident:
				isExec = execNames[file] == "."
			}
			index := 0
			if name == "CommandContext" {
				index = 1
			}
			if isExec && (name == "Command" || name == "CommandContext") && len(call.Args) > index && removalString(call.Args[index]) == "git" {
				gitCalls[call] = true
			}
			return true
		})
	}
	wrappers := make(map[ast.Node]bool)
	for changed := true; changed; {
		changed = false
		for fn, bodyCalls := range calls {
			for _, call := range bodyCalls {
				if gitCalls[call] || wrappers[resolve(call.Fun)] {
					gitCalls[call] = true
					if !wrappers[fn] {
						wrappers[fn] = true
						changed = true
					}
				}
			}
		}
	}
	return gitCalls, slices
}

// Collect literal slice elements and append operands without interpreting Go
// expressions. Assignments are conservatively combined, without control flow.
func removalSliceStrings(expr ast.Expr, slices map[*ast.Object][]string) []string {
	var values []string
	switch expr := expr.(type) {
	case *ast.Ident:
		return slices[expr.Obj]
	case *ast.CompositeLit:
		typ, ok := expr.Type.(*ast.ArrayType)
		if !ok || typ.Len != nil {
			return nil
		}
		elt, ok := typ.Elt.(*ast.Ident)
		if !ok || elt.Name != "string" {
			return nil
		}
		for _, elt := range expr.Elts {
			if pair, ok := elt.(*ast.KeyValueExpr); ok {
				elt = pair.Value
			}
			if value := removalString(elt); value != "" {
				values = append(values, value)
			}
		}
	case *ast.CallExpr:
		if id, ok := expr.Fun.(*ast.Ident); !ok || id.Name != "append" {
			return nil
		}
		for _, arg := range expr.Args {
			values = append(values, removalSliceStrings(arg, slices)...)
			if value := removalString(arg); value != "" {
				values = append(values, value)
			}
		}
	}
	return values
}
