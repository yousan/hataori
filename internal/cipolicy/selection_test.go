package cipolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCompleteClassifierDefaultsUnknownToFull(t *testing.T) {
	for _, name := range []string{"internal/newauth/grants.go", "internal/plugin/plugin.go", "internal/people/people.go", "internal/forge/github/client.go", "internal/supervisor/asked.go", "docs/protocol.md", "docs/plugins.md", "docs/configuration.md", "docs/notes/agent-usage-limits/captures/codex-poll.jsonl", "docs/notes/codex-supervisor-1088/scrub_actual.py", "unknown.asset", "go.mod", ".github/workflows/ci.yml"} {
		p := Classify([]Change{{Path: name, Status: "modified"}}, true)
		if p.DocsOnly || !p.HighRisk || !p.Full {
			t.Fatal("unknown/authority/contract path cheapened", name, p)
		}
	}
	for _, changes := range [][]Change{{{Path: "README.md", Previous: "internal/plugin/plugin.go", Status: "renamed"}}, {{Path: "README.md", Status: "modified"}, {Path: "internal/plugin/plugin.go", Status: "removed"}}} {
		p := Classify(changes, true)
		if p.DocsOnly || !p.Full {
			t.Fatal("rename/deletion hid code", p)
		}
	}
	for _, changes := range [][]Change{{{Path: "README.md", Status: "unknown"}}, {{Path: "docs/new.md", Previous: "../outside", Status: "renamed"}}} {
		p := Classify(changes, true)
		if !p.Full || p.DocsOnly {
			t.Fatal(p)
		}
	}
	if p := Classify([]Change{{Path: "README.md", Status: "modified"}}, false); !p.Full || p.DocsOnly {
		t.Fatal("incomplete docs enumeration skipped", p)
	}
	if p := Classify([]Change{{Path: "internal/version/version.go", Status: "modified"}}, true); p.Full || p.HighRisk || p.DocsOnly {
		t.Fatal("known ordinary path lost targeted lane", p)
	}
}

func sourceRepo(t *testing.T) (string, func(...string) string, func(string, string)) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Selector test")
	git("config", "user.email", "selector@example.invalid")
	write("go.mod", "module example.test/project\n\ngo 1.27\n")
	write("internal/version/version.go", "package version\nconst Version = 1\n")
	write("internal/consumer/use.go", "package consumer\nimport _ \"example.test/project/internal/version\"\n")
	write("internal/higher/use_test.go", "package higher\nimport _ \"example.test/project/internal/consumer\"\n")
	for _, name := range []string{"chat", "control", "wire", "plugin"} {
		write("internal/"+name+"/smoke.go", "package "+name+"\n")
	}
	git("add", ".")
	git("commit", "-qm", "baseline")
	return repo, git, write
}

func TestVerifiedCompleteDiffAndReverseDependencySmokeSelection(t *testing.T) {
	repo, git, write := sourceRepo(t)
	base := git("rev-parse", "HEAD")
	write("internal/version/version.go", "package version\nconst Version = 2\n")
	git("add", ".")
	git("commit", "-qm", "ordinary code")
	write("README.md", "last commit is prose\n")
	git("add", ".")
	git("commit", "-qm", "prose")
	head := git("rev-parse", "HEAD")
	p := Select(repo, base, head)
	want := []string{"./internal/consumer", "./internal/higher", "./internal/version"}
	if p.DocsOnly || p.Full || !reflect.DeepEqual(p.Packages, want) || len(p.Smoke) != 4 {
		t.Fatal("complete diff/importer closure/smoke lost", p)
	}
	// Rename to a docs location still sees deletion of executable code and
	// derives importers from the union of old/new immutable graphs.
	git("mv", "internal/version/version.go", "moved.md")
	git("commit", "-qm", "move source to prose")
	p = Select(repo, base, git("rev-parse", "HEAD"))
	if p.DocsOnly || !reflect.DeepEqual(p.Packages, []string{"./internal/consumer", "./internal/higher"}) {
		t.Fatal("deleted package lost importers", p)
	}
	if p := Select(repo, "HEAD", head); !p.Full || !p.HighRisk {
		t.Fatal("mutable source accepted", p)
	}
}

func TestParseFailureUnknownGoAndMissingSmokeEscalate(t *testing.T) {
	for _, kind := range []string{"parse", "unknown-go", "smoke", "module"} {
		t.Run(kind, func(t *testing.T) {
			repo, git, write := sourceRepo(t)
			base := git("rev-parse", "HEAD")
			switch kind {
			case "parse":
				write("internal/version/version.go", "package version\nimport (not valid)\n")
			case "unknown-go":
				write("internal/newauth/grants.go", "package newauth\n")
			case "smoke":
				git("rm", "internal/wire/smoke.go")
				write("internal/version/version.go", "package version\nconst Version=2\n")
			case "module":
				git("rm", "go.mod")
			}
			git("add", ".")
			git("commit", "-qm", "unsafe impact")
			p := Select(repo, base, git("rev-parse", "HEAD"))
			if !p.Full || !p.HighRisk || p.DocsOnly {
				t.Fatal(kind, p)
			}
		})
	}
}

func TestCompleteDiffBeyondPullFilesAPICap(t *testing.T) {
	repo, git, write := sourceRepo(t)
	base := git("rev-parse", "HEAD")
	for i := range 3005 {
		write("docs/page-"+strconv.Itoa(i)+".md", "ordinary prose\n")
	}
	git("add", ".")
	git("commit", "-qm", "large documentation change")
	changes, err := CompleteDiff(repo, base, git("rev-parse", "HEAD"))
	if err != nil || len(changes) != 3005 || !Classify(changes, true).DocsOnly {
		t.Fatal("large complete documentation range", len(changes), err)
	}
	// Sorting after every docs path reproduces the unsafe REST-cap boundary.
	// The final prose commit must not hide that earlier executable change.
	write("zz-runtime.go", "package runtime\n")
	git("add", ".")
	git("commit", "-qm", "late-sorted executable change")
	write("README.md", "final prose\n")
	git("add", ".")
	git("commit", "-qm", "final documentation")
	changes, err = CompleteDiff(repo, base, git("rev-parse", "HEAD"))
	if err != nil || len(changes) != 3007 || changes[len(changes)-1].Path != "zz-runtime.go" {
		t.Fatal("complete range truncated", len(changes), err)
	}
	if p := Classify(changes, true); p.DocsOnly || !p.Full || !p.HighRisk {
		t.Fatal("code beyond API cap skipped", p)
	}
	if p := Select(repo, base, git("rev-parse", "HEAD")); p.DocsOnly || !p.Full {
		t.Fatal("selector lost full immutable range", p)
	}
}
