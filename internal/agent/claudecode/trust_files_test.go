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
	return replayTrustOf(t, "2.1.328", name, spec)
}

// replayTrustOf is replayTrust for a capture of version.
func replayTrustOf(t *testing.T, version, name string, spec agent.Spec) (keys string, events []agent.Event) {
	t.Helper()
	spec.Dir = "/tmp/capture-work/sessions/" + name
	c := (&Adapter{}).NewCodec(spec).(*codec)
	c.Open()
	chunks, _ := filepath.Glob(filepath.Join("testdata", version, name, "screen-*.raw"))
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
				!strings.Contains(events[0].Text, "Trusting this folder loads") {
				t.Fatalf("events %+v; want one SessionEnded, blocked at start, naming %s", events, tc.refused)
			}
			if !slices.Contains(events[0].Trust, tc.refused) {
				t.Fatalf("trust %q; want every file the dialog names", events[0].Trust)
			}
		})
	}
}

// The dialog of Claude Code 2.1.329 whose project settings declare hooks
// draws a warning line above the line naming the files (#592): it is read
// like 2.1.328's, answered when the scope trusts the file and otherwise
// refused at once, naming it, with the warning quoted.
func TestTrustDialogWithTheHooksWarning(t *testing.T) {
	trusted := agent.Spec{TrustFiles: []string{".claude/settings.json"}}
	keys, events := replayTrustOf(t, "2.1.329", "trust_hooks_refused", agent.Spec{})
	if keys != "" || len(events) != 1 || events[0].Reason != agent.BlockedAtStart ||
		!slices.Equal(events[0].Trust, []string{".claude/settings.json"}) ||
		!strings.Contains(events[0].Text, "> ⚠ This folder configures hooks that can run commands without asking, declared\n> in .claude/settings.json\n") {
		t.Fatalf("refused: keys %q events %+v", keys, events)
	}
	if keys, events := replayTrustOf(t, "2.1.329", "trust_hooks_allowed", trusted); keys != "down enter" || len(events) != 0 {
		t.Fatalf("allowed: keys %q events %+v", keys, events)
	}
	// The refused capture ends with the dialog: only the first key goes.
	if keys, events := replayTrustOf(t, "2.1.329", "trust_hooks_refused", trusted); keys != "down" || len(events) != 0 {
		t.Fatalf("refused capture, trusted: keys %q events %+v", keys, events)
	}
}

// The warning about tools the project settings allow (#596) is read the
// same way: the rules are quoted and not read.
func TestTrustDialogWithThePermissionsWarning(t *testing.T) {
	trust := func(files ...string) agent.Spec { return agent.Spec{TrustFiles: files} }
	const one = "> ⚠ This folder pre-approves 1 tool permission in .claude/settings.json:\n> Bash(ls:*)\n" +
		"> These will apply without asking. Only proceed if you trust this configuration.\n" +
		"> Trusting this folder loads .claude/settings.json.\n"
	const both = "> ⚠ This folder pre-approves 10 tool permissions in .claude/settings.json and\n> .claude/settings.local.json:\n" +
		"> Bash(ls:*), Bash(a, b and c), Bash(git diff:*), Bash(git log:*), Bash(git\n" +
		"> show:*), Bash(git status:*), Bash(cat:*), Trusting this folder loads x., and 1\n> more\n"
	for i, tc := range []struct {
		version, capture string
		spec             agent.Spec
		keys             string   // sent, when answered
		refused          []string // named by the event, when refused
		quoted           string
	}{
		{"2.1.331", "trust_permissions_refused", agent.Spec{}, "", []string{".claude/settings.json"}, one},
		{"2.1.331", "trust_permissions_allowed", trust(".claude/settings.json"), "down enter", nil, ""},
		{"2.1.331", "trust_permissions_allowed", trust(".claude/settings.local.json"), "", []string{".claude/settings.json"}, one},
		{"2.1.331", "trust_permissions_allowed", agent.Spec{Confine: &agent.Confinement{}}, "down enter", nil, ""},
		{"2.1.331", "trust_permissions_allowed", agent.Spec{NoProjectSettings: true}, "down enter", nil, ""},
		// These captures end with the dialog: only the first key goes.
		{"2.1.331", "trust_permissions_refused", trust(".claude/settings.json"), "down", nil, ""},
		{"2.1.329", "trust_permissions", trust(".claude/settings.json"), "down", nil, ""},
		{"2.1.329", "trust_permissions", agent.Spec{}, "", []string{".claude/settings.json"}, one},
		// Two files, and a rule that writes the dialog's own line.
		{"2.1.331", "trust_permissions_both", trust(".claude/settings.json", ".claude/settings.local.json"), "down enter", nil, ""},
		{"2.1.331", "trust_permissions_both", trust(".claude/settings.json", "x"), "", []string{".claude/settings.json", ".claude/settings.local.json"}, both},
		{"2.1.331", "trust_permissions_both", agent.Spec{}, "", []string{".claude/settings.json", ".claude/settings.local.json"}, both},
		// Another warning between the rules and the sentence: not known.
		{"2.1.331", "trust_permissions_dirs", trust(".claude/settings.json"), "", nil, ""},
		{"2.1.331", "trust_permissions_dirs", agent.Spec{}, "", nil, ""},
		{"2.1.331", "trust_permissions_dirs", agent.Spec{Confine: &agent.Confinement{}}, "", nil, ""},
	} {
		keys, events := replayTrustOf(t, tc.version, tc.capture, tc.spec)
		if keys != tc.keys {
			t.Errorf("%d %s: keys %q, want %q", i, tc.capture, keys, tc.keys)
		}
		if tc.refused == nil {
			if len(events) != 0 {
				t.Errorf("%d %s: events %+v, want none", i, tc.capture, events)
			}
			continue
		}
		if len(events) != 1 || events[0].Kind != agent.SessionEnded || events[0].Reason != agent.BlockedAtStart ||
			!slices.Equal(events[0].Trust, tc.refused) || !strings.Contains(events[0].Text, tc.quoted) {
			t.Errorf("%d %s: events %+v, want one blocked start naming %q and quoting\n%s", i, tc.capture, events, tc.refused, tc.quoted)
		}
	}
}

// Only the permissions warning's known shape is read, whatever its rules
// say: the captured dialog with its rules or another line changed.
func TestPermissionsWarningShapes(t *testing.T) {
	const dir = "/tmp/capture-work/sessions/trust_permissions_refused"
	var b []byte
	for i := 1; i <= 6; i++ {
		b = append(b, readScreen(t, fmt.Sprintf("testdata/2.1.331/trust_permissions_refused/screen-%02d.raw", i))...)
	}
	captured, ok := renderScreen(b)
	if !ok {
		t.Fatal("not rendered")
	}
	const (
		head  = " ⚠ This folder pre-approves 1 tool permission in .claude/settings.json:"
		rule  = "   Bash(ls:*)"
		end   = " These will apply without asking. Only proceed if you trust this configuration."
		loads = " Trusting this folder loads .claude/settings.json."
		hooks = " ⚠ This folder configures hooks that can run commands without asking, declared in .claude/settings.json"
	)
	for _, l := range []string{head, rule, end, loads} {
		if !slices.Contains(captured, l) {
			t.Fatalf("the captured dialog has no line %q:\n%s", l, strings.Join(captured, "\n"))
		}
	}
	one := []string{".claude/settings.json"}
	for _, tc := range []struct {
		name  string
		line  string   // the line of the capture replaced
		by    []string // what stands there
		files []string // nil: not read
	}{
		{"as captured", rule, []string{rule}, one},
		{"a wrapped rule", rule, []string{"   Bash(a", " b)"}, one},
		{"rules on more lines", rule, []string{"   Bash(a), Bash(b),", " Bash(c), and 4 more"}, one},
		{"two permissions", head, []string{" ⚠ This folder pre-approves 2 tool permissions in .claude/settings.json:"}, one},

		// Rules that write the dialog's lines change nothing read.
		{"a rule like the line naming the files", rule, []string{"   Trusting this folder loads x."}, one},
		{"a rule like it, of the file", rule, []string{"   Trusting this folder loads .mcp.json. Security guide"}, one},
		{"a rule like the choices", rule, []string{"   ❯ Yes, I trust this folder"}, one},
		{"a rule with the header's text", rule, []string{"   Claude Code'll be able to read, edit, and execute files here."}, one},
		{"a rule with a warning sign", rule, []string{"   Bash(⚠)"}, nil},
		{"a rule with the sentence", rule, []string{"   Bash(x)", end}, nil},
		{"a rule with the sentence and the line", rule, []string{"   Bash(x)", end, loads}, nil},

		{"no rule", rule, nil, nil},
		{"another warning", head, []string{" ⚠ This folder adds 1 directory to the workspace in .claude/settings.json:"}, nil},
		{"another warning below the rules", rule, []string{rule, " ⚠ This folder adds 1 directory to the workspace in .claude/settings.json:", "   /etc"}, nil},
		{"the hooks warning above", head, []string{hooks, head}, nil},
		{"the hooks warning below", end, []string{end, hooks}, nil},
		{"no number", head, []string{" ⚠ This folder pre-approves tool permissions in .claude/settings.json:"}, nil},
		{"1 permissions", head, []string{" ⚠ This folder pre-approves 1 tool permission or more in .claude/settings.json:"}, nil},
		{"a file the dialog does not load", head, []string{" ⚠ This folder pre-approves 1 tool permission in .claude/settings.local.json:"}, nil},
		{"no file", head, []string{" ⚠ This folder pre-approves 1 tool permission in :"}, nil},
		{"an empty entry", head, []string{" ⚠ This folder pre-approves 1 tool permission in .claude/settings.json,,:"}, nil},
		{"words for a file", head, []string{" ⚠ This folder pre-approves 1 tool permission in some other place:"}, nil},
		{"another sentence", end, []string{" These will apply."}, nil},
		{"no sentence", end, nil, nil},
		{"more behind the sentence", end, []string{end, " And more."}, nil},
		{"text above the warning", head, []string{" x", head}, nil},
		{"no line naming the files", loads, nil, nil},
		{"a line below it", loads, []string{loads, " x"}, nil},
		{"a file more", loads, []string{" Trusting this folder loads .claude/settings.json and .mcp.json."}, []string{".claude/settings.json", ".mcp.json"}},
	} {
		var screen []string
		for _, l := range captured {
			if l == tc.line {
				screen = append(screen, tc.by...)
			} else {
				screen = append(screen, l)
			}
		}
		cursor, yes, files, ok := trustDialog(screen, dir)
		if ok != (tc.files != nil) || !slices.Equal(files, tc.files) || (ok && (cursor != 0 || yes != 1)) {
			t.Errorf("%s: ok %v, cursor %d, yes %d, files %q; want %q", tc.name, ok, cursor, yes, files, tc.files)
		}
	}
}

// The line 2.1.331 prints above the dialog when its last full-screen start
// did not finish (#707): the dialog below is read as it is without it, and
// a refused one is quoted without it.
func TestTrustDialogBelowTheFullscreenNotice(t *testing.T) {
	trusted := agent.Spec{TrustFiles: []string{".claude/settings.json"}}
	if keys, events := replayTrustOf(t, "2.1.331", "trust_notice_allowed", agent.Spec{}); keys != "down enter" || len(events) != 0 {
		t.Fatalf("allowed: keys %q events %+v", keys, events)
	}
	keys, events := replayTrustOf(t, "2.1.331", "trust_notice_refused", agent.Spec{})
	if keys != "" || len(events) != 1 || events[0].Reason != agent.BlockedAtStart ||
		!slices.Equal(events[0].Trust, []string{".claude/settings.json"}) ||
		!strings.Contains(events[0].Text, "Claude Code showed:\n> Claude Code'll be able to read") ||
		strings.Contains(events[0].Text, "fullscreen") {
		t.Fatalf("refused: keys %q events %+v", keys, events)
	}
	// The refused capture ends with the dialog: only the first key goes.
	if keys, events := replayTrustOf(t, "2.1.331", "trust_notice_refused", trusted); keys != "down" || len(events) != 0 {
		t.Fatalf("refused capture, trusted: keys %q events %+v", keys, events)
	}
}

// Only that line, once, right above the dialog's top rule: the captured
// dialog with anything else above the rule, or with a line added below it,
// is a screen the codec does not know.
func TestNoticeAboveTheTrustDialogShapes(t *testing.T) {
	const dir = "/tmp/capture-work/sessions/trust_notice_allowed"
	var b []byte
	for i := 1; i <= 6; i++ { // the dialog, with the cursor on "No, exit"
		b = append(b, readScreen(t, fmt.Sprintf("testdata/2.1.331/trust_notice_allowed/screen-%02d.raw", i))...)
	}
	screen, ok := renderScreen(b)
	if !ok || len(screen) == 0 || strings.TrimSpace(screen[0]) != fullscreenNotice {
		t.Fatalf("rendered %v, first line %q", ok, screen[:min(1, len(screen))])
	}
	rule := slices.IndexFunc(screen, func(l string) bool { return isRule(strings.TrimSpace(l)) })
	header := slices.IndexFunc(screen, func(l string) bool { return strings.TrimSpace(l) == "Accessing workspace:" })
	if rule < 1 || header != rule+1 {
		t.Fatalf("rule on line %d, header on %d", rule, header)
	}
	notice, dialog := screen[:rule], screen[rule:]
	const other = "Claude Code's fullscreen renderer has repeatedly failed to start on this machine, so it has been turned off here. " +
		"Run /tui fullscreen to try it again (this also resets after an update)."
	for _, tc := range []struct {
		name   string
		screen []string
		ok     bool
	}{
		{"as captured", screen, true},
		{"without the notice", dialog, true},
		{"another line in its place", slices.Concat([]string{other}, dialog), false},
		{"a line above it", slices.Concat([]string{"x"}, screen), false},
		{"a line between it and the rule", slices.Concat(notice, []string{"x"}, dialog), false},
		{"the notice twice", slices.Concat(notice, screen), false},
		{"more on its line", slices.Concat([]string{fullscreenNotice + " x"}, dialog), false},
		{"only a part of it", slices.Concat([]string{fullscreenNotice[:40]}, dialog), false},
		{"no rule below it", slices.Concat(notice, dialog[1:]), false},
		{"a line below the rule", slices.Concat(notice, dialog[:1], []string{"x"}, dialog[1:]), false},
		{"the notice below the rule", slices.Concat(dialog[:1], notice, dialog[1:]), false},
		{"the notice on both sides of the rule", slices.Concat(notice, dialog[:1], notice, dialog[1:]), false},
		{"a line below the header", slices.Concat(notice, dialog[:2], []string{"x"}, dialog[2:]), false},
	} {
		cursor, yes, files, ok := trustDialog(tc.screen, dir)
		if ok != tc.ok || (ok && (cursor != 0 || yes != 1 || len(files) != 0)) {
			t.Errorf("%s: ok %v, cursor %d, yes %d, files %q; want ok %v", tc.name, ok, cursor, yes, files, tc.ok)
		}
	}
}

// Only the warning's known shape is read: one that names a file the dialog
// does not load, or says something else, leaves the dialog unanswered.
func TestHooksWarningShapes(t *testing.T) {
	dir := "/w"
	head := "Accessing workspace: /w Quick safety check: Is this a project you created or one you trust? " +
		"(Like your own code, a well-known open source project, or work from your team). If not, " +
		"take a moment to review what's in this folder first. Claude Code'll be able to read, edit, and execute files here. "
	tail := " Security guide"
	for _, tc := range []struct {
		middle string
		files  []string
		ok     bool
	}{
		{hooksWarning + ".claude/settings.json Trusting this folder loads .claude/settings.json (runs commands).", []string{".claude/settings.json"}, true},
		{hooksWarning + ".claude/settings.local.json Trusting this folder loads .claude/settings.json (runs commands).", nil, false},
		{"⚠ This folder configures something else in .claude/settings.json Trusting this folder loads .claude/settings.json.", nil, false},
		{hooksWarning + ".claude/settings.json", nil, false},
		{hooksWarning + ".claude/settings.json,, Trusting this folder loads .claude/settings.json (runs commands).", nil, false},
		{hooksWarning + ".claude/settings.json and .claude/settings.local.json Trusting this folder loads .claude/settings.json and .claude/settings.local.json (runs commands).",
			[]string{".claude/settings.json", ".claude/settings.local.json"}, true},
	} {
		files, ok := headerMatches(head+tc.middle+tail, dir)
		if ok != tc.ok || !slices.Equal(files, tc.files) {
			t.Errorf("%q: %q %v, want %q %v", tc.middle, files, ok, tc.files, tc.ok)
		}
	}
}

// The text of a blocked start, for a person (#589): why hataori stopped,
// and the dialog as the CLI drew it with the files it names. The next step
// is the front end's (chat: TestABlockedStartsMessageSaysWhatToDoNext).
func TestBlockedStartText(t *testing.T) {
	_, events := replayTrust(t, "trust_files_refused", agent.Spec{})
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	text := events[0].Text
	for _, want := range []string{
		"because the dialog names files of the repository: trusting the folder lets the repository's own settings run commands without asking",
		"hataori stops for any such file, whatever it holds; it has not judged this repository dangerous.",
		"Claude Code showed:\n> Claude Code'll be able to read, edit, and execute files here.\n> Trusting this folder loads .claude/settings.json.\n",
		"\n> ❯ No, exit\n> Yes, I trust this folder\n> Enter to confirm · Esc to cancel",
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
// part that starts with a dot, or anything in docs/ at its top, is refused; other writes there are allowed
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
		// docs/ at its top is hataori's (#640); a docs/ further down is not.
		{"Write", "file_path", "/tmp/capture-work/docs/design.md", dir, dir, true},
		{"Write", "file_path", "docs/new.md", dir, dir, true},
		{"Edit", "file_path", "/tmp/capture-work/docs/sub/rules.md", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/Docs/design.md", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/docs", dir, dir, true},
		{"Write", "file_path", "/tmp/capture-work/notes/../docs/a.md", dir, dir, true},
		{"Write", "file_path", "/private/var/w/docs/a.md", "/private/var/w", "/var/w", true},
		{"Write", "file_path", "/tmp/capture-work/notes/docs/a.md", dir, dir, false},
		{"Write", "file_path", "/tmp/capture-work/docs.md", dir, dir, false},
		{"Write", "file_path", "/tmp/capture-work/mydocs/a.md", dir, dir, false},
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

// A launch without the project's settings (#590) leaves them out with
// --setting-sources user, a --resume too, and its codec answers a dialog
// that names only the settings files; one that names .mcp.json is still
// refused unless the scope trusts it.
func TestNoProjectSettings(t *testing.T) {
	spec := agent.Spec{Dir: "/w", StateDir: "/s", NoProjectSettings: true}
	for _, resume := range []string{"", "00000000-0000-4000-8000-000000000001"} {
		spec.ResumeID = resume
		l, err := (&Adapter{command: "claude", hataori: "hataori", environ: func() []string { return nil }}).Launch(spec)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.Index(l.Argv, "--setting-sources"); i < 0 || l.Argv[i+1] != "user" || !l.NoProjectSettings {
			t.Fatalf("resume %q: argv %q", resume, l.Argv)
		}
	}
	l, _ := (&Adapter{command: "claude", hataori: "hataori", environ: func() []string { return nil }}).Launch(agent.Spec{Dir: "/w", StateDir: "/s"})
	if slices.Contains(l.Argv, "--setting-sources") || l.NoProjectSettings {
		t.Fatalf("an ordinary launch: %q", l.Argv)
	}
	without := agent.Spec{NoProjectSettings: true}
	if keys, events := replayTrustOf(t, "2.1.329", "trust_hooks_refused", without); keys != "down" || len(events) != 0 {
		t.Fatalf("hooks, without settings: keys %q events %+v", keys, events)
	}
	if keys, events := replayTrust(t, "trust_files_both", without); keys != "down enter" || len(events) != 0 {
		t.Fatalf("both settings files, without settings: keys %q events %+v", keys, events)
	}
	if keys, events := replayTrust(t, "trust_files_mcp", without); keys != "" || len(events) != 1 {
		t.Fatalf(".mcp.json, without settings: keys %q events %+v", keys, events)
	}
}

// A link in a confined session's directory does not open docs/, a path
// with a part that starts with a dot, or a place outside to its writes: a
// path counts as where it leads (#640). A link that leads nowhere counts
// too, since a write would follow it.
func TestConfinedWriteThroughALink(t *testing.T) {
	b, err := os.ReadFile("testdata/2.1.327/confine_files_write_inside/hooks/04-PermissionRequest.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	dir, elsewhere := t.TempDir(), t.TempDir()
	for _, d := range []string{"docs", ".claude", "notes"} {
		os.Mkdir(filepath.Join(dir, d), 0o700)
	}
	os.WriteFile(filepath.Join(dir, "docs", "design.md"), []byte("x"), 0o444)
	for link, to := range map[string]string{
		"notes/ref":       "../docs",
		"notes/cfg":       "../.claude",
		"notes/out":       elsewhere,
		"notes/design.md": "../docs/design.md",
		"notes/new.md":    "../docs/new.md",
		"notes/mine":      ".",
	} {
		if err := os.Symlink(to, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for path, refused := range map[string]bool{
		"notes/ref/design.md":     true,
		"notes/cfg/settings.json": true,
		"notes/out/a.md":          true,
		"notes/design.md":         true,
		"notes/new.md":            true,
		"notes/mine/a.md":         false,
		"notes/plain.md":          false,
		"plain.md":                false,
	} {
		q := map[string]any{}
		for k, v := range p {
			q[k] = v
		}
		q["tool_name"], q["tool_input"], q["cwd"] = "Write", map[string]any{"file_path": filepath.Join(dir, path), "content": "x"}, dir
		raw, _ := json.Marshal(q)
		c := (&Adapter{}).NewCodec(agent.Spec{Dir: dir, Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
		a := c.Decode(agent.Hook, envelope("p1", "", raw))
		if len(a.Reply) != 1 {
			t.Fatalf("%q: %d replies", path, len(a.Reply))
		}
		if got := strings.Contains(string(a.Reply[0].Body), `"deny"`); got != refused {
			t.Errorf("%q: %s; want refused %v", path, a.Reply[0].Body, refused)
		}
	}
}
