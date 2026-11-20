package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// exampleDir is the example plugin of the repository (#353), relative to
// this package.
const exampleDir = "../../examples/plugins/marker"

// examplePlugin returns the example plugin's executable, absolute, and its
// SHA-256. The example is Python 3: without python3 on PATH the test fails,
// and does not skip, so that CI that lost the runtime says so. CI sets it
// up (.github/workflows/ci.yml).
func examplePlugin(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("the example plugin needs python3 on PATH, and there is none: %v", err)
	}
	path, err := filepath.Abs(filepath.Join(exampleDir, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return path, hex.EncodeToString(sum[:])
}

// The example plugin, started by the daemon with the fake surface and the
// fake agent, connects with its token, calls Threads, and reads events
// from the position it keeps in its state directory.
func TestExamplePluginRuns(t *testing.T) {
	path, sum := examplePlugin(t)
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{{"id": "marker", "path": path, "sha256": sum, "scopes": []string{"c1"}}}
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	eventually(t, "the plugin to call Threads", 20*time.Second, func() bool {
		return strings.Contains(logged(), "marker: threads 0")
	})
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	id, err := owner.Start(context.Background(), control.StartRequest{Key: "k1", Scope: "c1", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply hello"}})
	if err != nil {
		t.Fatal(err)
	}
	var end uint64
	eventually(t, "the turn's end", 20*time.Second, func() bool {
		d, err := owner.Thread(context.Background(), id, 0, control.MaxEvents)
		if err != nil {
			return false
		}
		for _, e := range d.Events {
			if e.Kind == "turn_ended" {
				end = e.Pos
			}
		}
		return end > 0
	})
	position := filepath.Join(r.root, "state", "plugins", "marker", "position")
	eventually(t, "the plugin to keep the turn end's position", 20*time.Second, func() bool {
		b, err := os.ReadFile(position)
		n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return err == nil && n >= end
	})
	if l := logged(); !strings.Contains(l, "marker: connected; the daemon speaks "+wire.Version) ||
		!strings.Contains(l, "marker: event turn_ended "+strconv.FormatUint(end, 10)) {
		t.Fatalf("the plugin's log lines:\n%s", l)
	}
}

// The example plugin imports nothing outside Python's standard library:
// python3 parses it and checks each module it imports against the list of
// its own standard library (Python 3.10 and later).
func TestExamplePluginNeedsOnlyTheStandardLibrary(t *testing.T) {
	path, _ := examplePlugin(t)
	const check = `
import ast, sys
tree = ast.parse(open(sys.argv[1], encoding="utf-8").read())
mods = set()
for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        mods.update(a.name.split(".")[0] for a in node.names)
    elif isinstance(node, ast.ImportFrom) and node.level == 0:
        mods.add(node.module.split(".")[0])
outside = sorted(m for m in mods if m not in sys.stdlib_module_names)
print("imports:", " ".join(sorted(mods)))
if outside:
    print("outside the standard library:", " ".join(outside))
    sys.exit(1)
`
	out, err := exec.Command("python3", "-I", "-c", check, path).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "imports: json") {
		t.Fatalf("the check read no imports: %s", out)
	}
}
