package cipolicy

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
)

type Change struct{ Path, Previous, Status string }
type Plan struct {
	DocsOnly, Full, HighRisk, Compatibility bool
	Packages, Smoke, Reasons                []string
}

// Classification is compiled control code, never a candidate's paths filter.
// Unknown paths and incomplete enumerations escalate; rename/delete inspect
// both old and new locations. Review requirements are additional to test scope.
func Classify(changes []Change, complete bool) Plan {
	p := Plan{DocsOnly: true}
	if !complete {
		return Plan{Full: true, HighRisk: true, Compatibility: true, Reasons: []string{"incomplete diff"}}
	}
	for _, c := range changes {
		if !safePath(c.Path) || (c.Status != "added" && c.Status != "modified" && c.Status != "removed" && c.Status != "renamed") || (c.Status == "renamed" && !safePath(c.Previous)) {
			return Plan{Full: true, HighRisk: true, Compatibility: true, Reasons: []string{"unknown diff entry"}}
		}
		paths := []string{c.Path}
		if c.Previous != "" {
			paths = append(paths, c.Previous)
		}
		for _, name := range paths {
			if documentation(name) {
				continue
			}
			p.DocsOnly = false
			if strings.HasPrefix(name, ".github/") || name == "AGENTS.md" || name == "CLAUDE.md" || name == "go.mod" || name == "go.sum" ||
				strings.HasPrefix(name, "internal/holder/") || strings.HasPrefix(name, "internal/hostterm/") || strings.HasPrefix(name, "internal/runner/") {
				p.Compatibility = true
			}
			if !ordinaryFile(name) {
				p.Full, p.HighRisk = true, true
				p.Reasons = append(p.Reasons, "high-risk or unclassified: "+name)
			}
		}
	}
	return p
}

func documentation(p string) bool {
	if p == "AGENTS.md" || p == "CLAUDE.md" || p == "docs/testing.md" || strings.HasPrefix(p, "docs/design/ci-policy") || p == "docs/protocol.md" || p == "docs/plugins.md" || p == "docs/configuration.md" {
		return false
	}
	ext := strings.ToLower(path.Ext(p))
	if path.Dir(p) == "." && (ext == ".md" || ext == ".markdown" || p == "LICENSE") {
		return true
	}
	if strings.HasPrefix(p, "docs/") && (ext == ".md" || ext == ".markdown" || ext == ".png" || ext == ".jpg" || ext == ".svg") {
		return true
	}
	if strings.HasPrefix(p, "site/") && (ext == ".md" || ext == ".css" || ext == ".html" || ext == ".svg" || ext == ".png" || ext == ".jpg") {
		return true
	}
	return false
}

type gitEntry struct{ SHA, Mode string }
type gitTree struct {
	repo, revision string
	files          map[string]gitEntry
}

func objectHash(kind string, b []byte, sha string) string {
	header := []byte(fmt.Sprintf("%s %d\x00", kind, len(b)))
	payload := append(header, b...)
	if len(sha) == 64 {
		v := sha256.Sum256(payload)
		return hex.EncodeToString(v[:])
	}
	v := sha1.Sum(payload)
	return hex.EncodeToString(v[:])
}
func gitObject(repo, kind, sha string) ([]byte, error) {
	if !revisionOK(sha) {
		return nil, fmt.Errorf("mutable Git object")
	}
	b, err := exec.Command("git", "-C", repo, "cat-file", kind, sha).Output()
	if err != nil {
		return nil, err
	}
	if objectHash(kind, b, sha) != sha {
		return nil, fmt.Errorf("Git object digest mismatch")
	}
	return b, nil
}
func snapshot(repo, revision string) (gitTree, error) {
	t := gitTree{repo: repo, revision: revision, files: map[string]gitEntry{}}
	b, err := gitObject(repo, "commit", revision)
	if err != nil {
		return t, err
	}
	first, _, ok := bytes.Cut(b, []byte("\n"))
	if !ok || !bytes.HasPrefix(first, []byte("tree ")) {
		return t, fmt.Errorf("invalid commit tree")
	}
	root := string(first[5:])
	hashLen := len(revision) / 2
	var walk func(string, string, int) error
	walk = func(sha, prefix string, depth int) error {
		if depth > 100 || len(t.files) > 100000 {
			return fmt.Errorf("tree enumeration bound exceeded")
		}
		data, err := gitObject(repo, "tree", sha)
		if err != nil {
			return err
		}
		for len(data) > 0 {
			header, rest, ok := bytes.Cut(data, []byte{0})
			if !ok || len(rest) < hashLen {
				return fmt.Errorf("truncated Git tree")
			}
			mode, name, ok := strings.Cut(string(header), " ")
			if !ok || strings.Contains(name, "/") {
				return fmt.Errorf("invalid tree entry")
			}
			p := prefix + name
			if !safePath(p) {
				return fmt.Errorf("unsafe tree entry")
			}
			object := hex.EncodeToString(rest[:hashLen])
			data = rest[hashLen:]
			if mode == "40000" {
				if err := walk(object, p+"/", depth+1); err != nil {
					return err
				}
			} else {
				if _, exists := t.files[p]; exists {
					return fmt.Errorf("duplicate tree entry")
				}
				t.files[p] = gitEntry{object, mode}
			}
		}
		return nil
	}
	return t, walk(root, "", 0)
}
func (t gitTree) read(p string) ([]byte, error) {
	e, ok := t.files[p]
	if !ok || (e.Mode != "100644" && e.Mode != "100755") {
		return nil, fmt.Errorf("source is missing/nonregular")
	}
	return gitObject(t.repo, "blob", e.SHA)
}

// CompleteDiff enumerates verified immutable trees without checkout, external
// diff drivers, hooks, rename limits or API file caps. Renames are represented
// conservatively as removal+addition, so neither old nor new path disappears.
func CompleteDiff(repo, base, head string) ([]Change, error) {
	a, err := snapshot(repo, base)
	if err != nil {
		return nil, err
	}
	b, err := snapshot(repo, head)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for p, old := range a.files {
		next, ok := b.files[p]
		if !ok {
			changes = append(changes, Change{Path: p, Status: "removed"})
		} else if old != next {
			changes = append(changes, Change{Path: p, Status: "modified"})
		}
	}
	for p := range b.files {
		if _, ok := a.files[p]; !ok {
			changes = append(changes, Change{Path: p, Status: "added"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// Select derives the entire diff itself. It reads only verified Git blobs and
// parses all Go import edges (including tests/platform files) without executing
// candidate go commands, generators, hooks, build scripts or provider code.
func Select(repo, base, head string) Plan {
	changes, err := CompleteDiff(repo, base, head)
	if err != nil {
		return Plan{Full: true, HighRisk: true, Compatibility: true, Reasons: []string{err.Error()}}
	}
	p := Classify(changes, true)
	if p.DocsOnly {
		return p
	}
	old, err := snapshot(repo, base)
	if err != nil {
		p.Full = true
		p.HighRisk = true
		return p
	}
	current, err := snapshot(repo, head)
	if err != nil {
		p.Full = true
		p.HighRisk = true
		return p
	}
	graphs := map[string]map[string]bool{}
	packages := map[string]bool{}
	for _, tree := range []gitTree{old, current} {
		mod, err := tree.read("go.mod")
		if err != nil {
			p.Full = true
			p.HighRisk = true
			p.Reasons = append(p.Reasons, "module source unavailable")
			return p
		}
		module := ""
		for _, line := range strings.Split(string(mod), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "module" {
				module = strings.Trim(fields[1], "\"")
			}
		}
		if module == "" {
			p.Full = true
			p.HighRisk = true
			return p
		}
		for name := range tree.files {
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			dir := path.Dir(name)
			pkg := "./" + dir
			if dir == "." {
				pkg = "."
			}
			packages[pkg] = true
			b, err := tree.read(name)
			if err != nil {
				p.Full = true
				p.HighRisk = true
				return p
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, b, parser.ImportsOnly)
			if err != nil {
				p.Full = true
				p.HighRisk = true
				p.Reasons = append(p.Reasons, "unparseable import graph")
				return p
			}
			for _, imp := range file.Imports {
				v, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					p.Full = true
					p.HighRisk = true
					return p
				}
				if v == module || strings.HasPrefix(v, module+"/") {
					dependency := strings.TrimPrefix(v, module)
					dependency = "." + dependency
					if graphs[dependency] == nil {
						graphs[dependency] = map[string]bool{}
					}
					graphs[dependency][pkg] = true
				}
			}
		}
	}
	affected := map[string]bool{}
	for _, c := range changes {
		for _, name := range []string{c.Path, c.Previous} {
			if name == "" || documentation(name) {
				continue
			}
			dir := path.Dir(name)
			pkg := "./" + dir
			if dir == "." {
				pkg = "."
			}
			if !packages[pkg] {
				p.Full = true
				p.HighRisk = true
				p.Reasons = append(p.Reasons, "unknown package impact: "+name)
			} else {
				affected[pkg] = true
			}
		}
	}
	for dep := range graphs {
		if !packages[dep] {
			p.Full = true
			p.HighRisk = true
			p.Reasons = append(p.Reasons, "missing internal dependency: "+dep)
		}
	}
	for changed := true; changed; {
		changed = false
		for dep, importers := range graphs {
			if affected[dep] {
				for importer := range importers {
					if !affected[importer] {
						affected[importer] = true
						changed = true
					}
				}
			}
		}
	}
	for pkg := range affected {
		if hasPackage(current, pkg) {
			p.Packages = append(p.Packages, pkg)
		}
	}
	sort.Strings(p.Packages)
	for _, pkg := range []string{"./internal/chat", "./internal/control", "./internal/wire", "./internal/plugin"} {
		if hasPackage(current, pkg) {
			p.Smoke = append(p.Smoke, pkg)
		} else {
			p.Full = true
			p.HighRisk = true
			p.Reasons = append(p.Reasons, "required cross-module smoke package absent: "+pkg)
		}
	}
	return p
}
func hasPackage(t gitTree, pkg string) bool {
	dir := strings.TrimPrefix(pkg, "./")
	if pkg == "." {
		dir = "."
	}
	for name := range t.files {
		if strings.HasSuffix(name, ".go") && path.Dir(name) == dir {
			return true
		}
	}
	return false
}

// New files/packages are high risk until a reviewed control revision explicitly
// classifies them. This small starting allowlist can grow with measured evidence.
func ordinaryFile(p string) bool {
	switch p {
	case "internal/version/version.go", "internal/version/version_test.go", "internal/chat/names.go", "internal/chat/names_test.go", "internal/chat/lamps.go", "internal/chat/lamps_test.go":
		return true
	}
	return false
}
