package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// replayTrust gives a codec for spec the screens of a capture under
// testdata/2.1.328/<name>, one chunk at a time with ticks a second apart
// in between, as the runner does, and returns the keys it sent and the
// events it produced.
func replayTrust(t *testing.T, name string, spec agent.Spec) (keys string, events []agent.Event) {
	t.Helper()
	spec.Dir = "/tmp/capture-work/sessions/" + name
	c := (&Adapter{}).NewCodec(spec).(*codec)
	c.Open()
	chunks, _ := filepath.Glob(filepath.Join("testdata/2.1.328", name, "screen-*.raw"))
	if len(chunks) == 0 {
		t.Fatalf("no screens in %s", name)
	}
	var all Actions
	now := time.Now()
	for _, p := range chunks {
		all = merge(all, c.Decode(agent.Screen, readScreen(t, p)))
		for range 4 {
			now = now.Add(time.Second)
			all = merge(all, c.Tick(now))
		}
	}
	return keysOf(all), all.Events
}

// The dialog that names a project settings file (Claude Code 2.1.328): a
// confined session gets it answered; an ordinary one only when the scope
// trusts every file it names, and otherwise fails at once, naming them.
func TestTrustDialogThatNamesFiles(t *testing.T) {
	confined := agent.Spec{Confine: &agent.Confinement{}}
	trust := func(files ...string) agent.Spec { return agent.Spec{TrustFiles: files} }
	for i, tc := range []struct {
		capture string
		spec    agent.Spec
		keys    string // the keys sent when answered
		refused string // the file the event names as not trusted; "" when answered
	}{
		{"trust_files_refused", agent.Spec{}, "", ".claude/settings.json"},
		{"trust_files_allowed", trust(".claude/settings.json"), "down enter", ""},
		{"trust_files_confined", confined, "down enter", ""},
		// The same screens with the other specs. The refused capture ends
		// with the dialog: no screen shows the cursor moved, so only the
		// first key goes.
		{"trust_files_refused", confined, "down", ""},
		{"trust_files_refused", trust(".claude/settings.json"), "down", ""},
		{"trust_files_refused", trust(".claude/settings.local.json"), "", ".claude/settings.json"},
		{"trust_files_allowed", agent.Spec{}, "", ".claude/settings.json"},
		{"trust_files_allowed", confined, "down enter", ""},
		{"trust_files_confined", agent.Spec{}, "", ".claude/settings.local.json"},
		{"trust_files_confined", trust(".claude/settings.json"), "", ".claude/settings.local.json"},
		{"trust_files_confined", trust(".claude/settings.json", ".claude/settings.local.json"), "down enter", ""},
		// Two files: both must be trusted. An MCP config named "here or above".
		{"trust_files_both", trust(".claude/settings.json", ".claude/settings.local.json"), "down enter", ""},
		{"trust_files_both", trust(".claude/settings.json"), "", ".claude/settings.local.json"},
		{"trust_files_both", confined, "down enter", ""},
		{"trust_files_mcp", agent.Spec{}, "", ".mcp.json"},
		{"trust_files_mcp", trust(".mcp.json"), "down", ""},
		{"trust_files_mcp", confined, "down", ""},
	} {
		t.Run(fmt.Sprintf("%d %s", i, tc.capture), func(t *testing.T) {
			keys, events := replayTrust(t, tc.capture, tc.spec)
			if tc.refused == "" {
				if keys != tc.keys || len(events) != 0 {
					t.Fatalf("keys %q, events %+v; want %q and no event", keys, events, tc.keys)
				}
				return
			}
			if keys != "" {
				t.Fatalf("keys %q for a dialog it may not answer", keys)
			}
			if len(events) != 1 || events[0].Kind != agent.SessionEnded || events[0].Reason != agent.BlockedAtStart ||
				!strings.Contains(events[0].Text, "to let "+tc.refused+" through") {
				t.Fatalf("events %+v; want one SessionEnded, blocked at start, naming %s", events, tc.refused)
			}
		})
	}
}

// The text of a blocked start, for a person (#589): why hataori stopped,
// the dialog as the CLI drew it with the files it names, and the next
// step, naming the scope.
func TestBlockedStartText(t *testing.T) {
	_, events := replayTrust(t, "trust_files_refused", agent.Spec{Scope: "C0123"})
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	text := events[0].Text
	for _, want := range []string{
		"because the dialog names files of the repository: trusting the folder lets the repository's own settings run commands without asking",
		"hataori stops for any such file, whatever it holds; it has not judged this repository dangerous.",
		"Claude Code showed:\n> Claude Code'll be able to read, edit, and execute files here.\n> Trusting this folder loads .claude/settings.json.\n",
		"\n> ❯ No, exit\n> Yes, I trust this folder\n",
		`What to do next: to let .claude/settings.json through for this scope's threads, list it under "trust_files" of the scope "C0123" in hataori's config, and start again.`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text has no %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\r") || strings.Contains(text, "Accessing workspace") {
		t.Errorf("the text holds escape codes or the header:\n%s", text)
	}
}

// A narrow terminal wraps the dialog's sentences: the quote keeps them.
func TestDialogBodyOfAWrappedDialog(t *testing.T) {
	screen := []string{"Accessing workspace:", "/w", "Quick safety check: ...", "", "Claude Code'll be able to read, edit, and execute", "files here.", "",
		"Trusting this folder loads", ".claude/settings.json.", "", "Security guide", "", "❯ No, exit", "  Yes, I trust this folder", "",
		"Enter to confirm · Esc to cancel"}
	got := strings.Join(dialogBody(screen), "|")
	want := "Claude Code'll be able to read, edit, and execute|files here.|Trusting this folder loads|.claude/settings.json.|Security guide|❯ No, exit|Yes, I trust this folder|Enter to confirm · Esc to cancel"
	if got != want {
		t.Fatalf("dialogBody = %q\nwant %q", got, want)
	}
}

// What the captured dialogs name, and the captured dialog of 2.1.322 that
// names nothing, read the same as before.
func TestTrustDialogFiles(t *testing.T) {
	for _, tc := range []struct {
		capture, dir string
		files        []string
	}{
		{"testdata/2.1.328/trust_files_refused", "/tmp/capture-work/sessions/trust_files_refused", []string{".claude/settings.json"}},
		{"testdata/2.1.328/trust_files_confined", "/tmp/capture-work/sessions/trust_files_confined", []string{".claude/settings.local.json"}},
		{"testdata/2.1.328/trust_files_both", "/tmp/capture-work/sessions/trust_files_both", []string{".claude/settings.json", ".claude/settings.local.json"}},
		{"testdata/2.1.328/trust_files_mcp", "/tmp/capture-work/sessions/trust_files_mcp", []string{".mcp.json"}},
		{"testdata/2.1.322/trust", trustDir, nil},
	} {
		var b []byte
		names, _ := filepath.Glob(filepath.Join(tc.capture, "screen-*.raw"))
		if len(names) == 0 {
			names = []string{filepath.Join(tc.capture, "terminal.raw")}
		}
		for _, n := range names {
			b = append(b, readScreen(t, n)...)
		}
		screen, ok := renderScreen(b)
		if !ok {
			t.Fatalf("%s: not rendered", tc.capture)
		}
		_, _, files, ok := trustDialog(screen, tc.dir)
		if !ok || !slices.Equal(files, tc.files) {
			t.Errorf("%s: files %q, ok %t; want %q", tc.capture, files, ok, tc.files)
		}
	}
}

// A confined session's permission request to write a settings, git or tool
// configuration file, or anything else whose path in its directory has a
// part that starts with a dot, is refused; other writes there are allowed
// as before, and an ordinary session's are all allowed.
func TestConfinedWriteToProtectedFiles(t *testing.T) {
	b, err := os.ReadFile("testdata/2.1.327/confine_files_write_inside/hooks/04-PermissionRequest.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	const dir = "/tmp/capture-work" // the capture's cwd
	request := func(tool, field, path, cwd string) []byte {
		q := map[string]any{}
		for k, v := range p {
			q[k] = v
		}
		q["tool_name"], q["tool_input"], q["cwd"] = tool, map[string]any{field: path, "content": "x"}, cwd
		raw, _ := json.Marshal(q)
		return envelope("p1", "", raw)
	}
	for _, tc := range []struct {
		tool, field, path, cwd, dir string
		refused                     bool
	}{
		{"Write", "file_path", "/tmp/capture-work/a.txt", dir, dir, false},
		{"Write", "file_path", "notes/today.md", dir, dir, false},
		{"Edit", "file_path", "/tmp/capture-work/notes/a.md", dir, dir, false},
		{"Write", "file_path", "/tmp/capture-work/.claude/settings.local.json", dir, dir, true},
		{"Write", "file_path", ".claude/settings.json", dir, dir, true},
		{"Edit", "file_path", "/tmp/capture-work/.git/config", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/.git/HEAD", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/.mcp.json", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/sub/.gitconfig", dir, dir, true},
		{"MultiEdit", "file_path", "/tmp/capture-work/.vscode/settings.json", dir, dir, true},
		{"NotebookEdit", "notebook_path", "/tmp/capture-work/.ipynb_checkpoints/a.ipynb", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/sub/../.git/config", dir, dir, true},
		{"Write", "file_path", "/tmp/other/a.txt", dir, dir, true},
		{"Write", "file_path", "../a.txt", dir, dir, true},
		{"Write", "file_path", "", dir, dir, true},
		// The CLI names the directory as it resolved it.
		{"Write", "file_path", "/private/var/w/a.txt", "/private/var/w", "/var/w", false},
		{"Write", "file_path", "/private/var/w/.git/config", "/private/var/w", "/var/w", true},
		// A cwd that is not the session's directory widens nothing.
		{"Write", "file_path", "/etc/x", "/", "/var/w", true},
		{"Write", "file_path", "/var/sibling/a.txt", "/var", "/var/w", true},
	} {
		c := (&Adapter{}).NewCodec(agent.Spec{Dir: tc.dir, Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
		a := c.Decode(agent.Hook, request(tc.tool, tc.field, tc.path, tc.cwd))
		if len(a.Reply) != 1 {
			t.Fatalf("%s %q: %d replies", tc.tool, tc.path, len(a.Reply))
		}
		var r struct {
			Out struct {
				Decision struct{ Behavior, Message string } `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		json.Unmarshal(a.Reply[0].Body, &r)
		want := "allow"
		if tc.refused {
			want = "deny"
		}
		if r.Out.Decision.Behavior != want || (tc.refused && !strings.Contains(r.Out.Decision.Message, "confined")) {
			t.Errorf("confined, %s %q: %s", tc.tool, tc.path, a.Reply[0].Body)
		}
		ordinary := (&Adapter{}).NewCodec(agent.Spec{Dir: tc.dir, Approvals: agent.AllowAll}).(*codec)
		if a := ordinary.Decode(agent.Hook, request(tc.tool, tc.field, tc.path, tc.cwd)); len(a.Reply) != 1 || string(a.Reply[0].Body) != string(allow) {
			t.Errorf("ordinary, %s %q: %q", tc.tool, tc.path, a.Reply)
		}
	}
	// Not a write: allowed in a confined session too.
	c := (&Adapter{}).NewCodec(agent.Spec{Dir: dir, Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	if a := c.Decode(agent.Hook, envelope("p2", "", b)); len(a.Reply) != 1 || string(a.Reply[0].Body) != string(allow) {
		t.Fatalf("the captured write inside: %q", a.Reply)
	}
	q := map[string]any{}
	for k, v := range p {
		q[k] = v
	}
	q["tool_name"], q["tool_input"] = "mcp__hataori__send", map[string]any{"thread": 2, "text": ".git"}
	raw, _ := json.Marshal(q)
	if a := c.Decode(agent.Hook, envelope("p3", "", raw)); len(a.Reply) != 1 || string(a.Reply[0].Body) != string(allow) {
		t.Fatalf("an MCP tool: %q", a.Reply)
	}
}
