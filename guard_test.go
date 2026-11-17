// Guard test for issue #24: nothing in phase 1 removes or resets a working
// directory.
//
// The test scans every non-test Go file in this module for calls that delete
// or reset work — os.Remove, os.RemoveAll, and git reset, git clean,
// git checkout -f, git stash drop and git worktree remove — and fails on any
// use that is not listed in allowListed below. allowListed is the single
// allow-list for these operations: add an entry only with a reason showing
// the call cannot touch a working directory.
//
// Files under testdata and nested modules (a directory with its own go.mod,
// such as docs/notes/sqlite-bench, which removes only the temporary
// directory it created) are not part of this module and are skipped.
package hataori_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowListed is the single allow-list for destructive operations in
// non-test code, keyed by path relative to the repository root and then by
// operation. The value explains why the call cannot lose work.
var allowListed = map[string]map[string]string{
	"internal/host/host.go": {
		"os.Remove": "removes only the daemon's own socket, its pointer file, and the empty directory made for that socket",
	},
	"internal/runner/runner.go": {
		"os.Remove": "removes only the stale status file of a host that has ended, before a session resumes",
	},
	"internal/wire/server.go": {
		"os.Remove": "removes only a stale socket and its pointer file while the instance lock is held",
	},
	"internal/workspace/mirror.go": {
		"os.RemoveAll": "removes only the half-built bare mirror in the temporary directory this call created under state/mirrors-tmp; a mirror is not a working directory and holds no one's work",
	},
}

// destructiveGitSubcommands are the git subcommands that reset or discard
// work, matched against the literal arguments of an exec.Command call.
var destructiveGitSubcommands = map[string]bool{
	"reset": true,
	"clean": true,
}

func TestNoDestructiveOperationOutsideAllowList(t *testing.T) {
	root := repoRoot(t)
	seen := map[string]map[string]bool{}
	var violations []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return walkDir(t, root, path, d)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		violations = append(violations, scanFile(t, path, rel, seen)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	for file, ops := range allowListed {
		for op := range ops {
			if !seen[file][op] {
				violations = append(violations, fmt.Sprintf(
					"allow-list entry for %s (%s) is no longer used; remove it from guard_test.go", file, op))
			}
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("destructive operation outside the allow-list (see guard_test.go):\n%s",
			strings.Join(violations, "\n"))
	}
}

// repoRoot returns the directory holding this test file, which is the
// repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Dir(file)
}

// walkDir decides whether to descend into dir: hidden directories, testdata
// and nested modules (a directory with its own go.mod) are skipped.
func walkDir(t *testing.T, root, path string, d os.DirEntry) error {
	t.Helper()
	if path == root {
		return nil
	}
	if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
		return filepath.SkipDir
	}
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return filepath.SkipDir
	}
	return nil
}

// scanFile reports the destructive operations in one non-test Go file that
// are not in the allow-list, and records the allow-listed ones it saw.
func scanFile(t *testing.T, path, rel string, seen map[string]map[string]bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		switch x.Name {
		case "os":
			if sel.Sel.Name == "Remove" || sel.Sel.Name == "RemoveAll" {
				op := "os." + sel.Sel.Name
				violations = append(violations, checkUse(rel, pos.Line, op, seen)...)
			}
		case "exec":
			if sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext" {
				violations = append(violations, checkGitCommand(call, sel.Sel.Name, rel, pos.Line, seen)...)
			}
		}
		return true
	})
	return violations
}

// checkUse records an allow-listed use, or reports it as a violation.
func checkUse(rel string, line int, op string, seen map[string]map[string]bool) []string {
	if allowListed[rel][op] != "" {
		if seen[rel] == nil {
			seen[rel] = map[string]bool{}
		}
		seen[rel][op] = true
		return nil
	}
	return []string{fmt.Sprintf("%s:%d: %s is not in the allow-list", rel, line, op)}
}

// checkGitCommand reports a destructive git subcommand run through
// exec.Command or exec.CommandContext with literal arguments.
func checkGitCommand(call *ast.CallExpr, name, rel string, line int, seen map[string]map[string]bool) []string {
	args := call.Args
	if name == "CommandContext" {
		if len(args) == 0 {
			return nil
		}
		args = args[1:]
	}
	lit := literalStrings(args)
	if len(lit) == 0 || lit[0] != "git" {
		return nil
	}
	op := ""
	switch {
	case len(lit) > 1 && destructiveGitSubcommands[lit[1]]:
		op = "git " + lit[1]
	case len(lit) > 1 && lit[1] == "checkout" && hasFlag(lit[2:], "-f", "--force"):
		op = "git checkout -f"
	case len(lit) > 1 && lit[1] == "stash" && hasFlag(lit[2:], "drop"):
		op = "git stash drop"
	case len(lit) > 1 && lit[1] == "worktree" && hasFlag(lit[2:], "remove"):
		op = "git worktree remove"
	}
	if op == "" {
		return nil
	}
	return checkUse(rel, line, op, seen)
}

// literalStrings returns the string-literal arguments, stopping at the first
// argument that is not a literal.
func literalStrings(args []ast.Expr) []string {
	var out []string
	for _, arg := range args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			break
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			break
		}
		out = append(out, s)
	}
	return out
}

// hasFlag reports whether any of args equals one of flags.
func hasFlag(args []string, flags ...string) bool {
	for _, arg := range args {
		for _, flag := range flags {
			if arg == flag {
				return true
			}
		}
	}
	return false
}
