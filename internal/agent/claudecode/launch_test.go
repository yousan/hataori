package claudecode

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

var update = flag.Bool("update", false, "rewrite the golden files of the launch")

// ordinaryLaunches is the launch of three ordinary specs, a new session
// with everything set, one with nothing set and a resumed one, with the
// new session's random id replaced. The paths are made up: Launch reads
// nothing at them but a user's status line, which they do not have.
func ordinaryLaunches(t *testing.T) []byte {
	t.Helper()
	a := newAdapter(t, `{"hataori": "/opt/hataori/bin/hataori", "pass_env": ["ANTHROPIC_API_KEY"]}`)
	a.environ = func() []string {
		return []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent/home", "LANG=C.UTF-8", "ANTHROPIC_API_KEY=k", "SECRET=s"}
	}
	specs := []agent.Spec{
		{Dir: "/nonexistent/workspaces/7", StateDir: "/nonexistent/state/sessions/7", Model: "opus", Effort: "high", Instructions: "Be brief."},
		{Dir: "/nonexistent/workspaces/8", StateDir: "/nonexistent/state/sessions/8"},
		{Dir: "/nonexistent/workspaces/7", StateDir: "/nonexistent/state/sessions/7", ResumeID: "00000000-0000-4000-8000-000000000007", Model: "opus"},
	}
	type file struct {
		Name string
		Text string
	}
	type launch struct {
		Argv     []string
		Env      []string
		Terminal bool
		Files    []file
		Confined bool `json:",omitempty"`
	}
	var out []launch
	for _, s := range specs {
		l, err := a.Launch(s)
		if err != nil {
			t.Fatal(err)
		}
		for i := range l.Argv {
			if i > 0 && l.Argv[i-1] == "--session-id" {
				l.Argv[i] = "<new session id>"
			}
		}
		got := launch{Argv: l.Argv, Env: l.Env, Terminal: l.Terminal}
		for _, name := range []string{SettingsFile, InstructionsFile, MCPConfigFile} {
			if b, ok := l.Files[name]; ok {
				got.Files = append(got.Files, file{name, string(b)})
			}
		}
		if len(got.Files) != len(l.Files) {
			t.Fatalf("launch has files %v", l.Files)
		}
		got.Confined = l.Confined
		out = append(out, got)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// The launch of a session that is not confined is what it was before
// confinement existed, byte for byte: the golden file was written by the
// adapter of the commit before it.
func TestOrdinaryLaunchUnchanged(t *testing.T) {
	const golden = "testdata/launch/ordinary.json"
	got := ordinaryLaunches(t)
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the ordinary launch changed:\n%s\nwant:\n%s", got, want)
	}
}

// A confined spec's launch is the ordinary launch with the flags that
// confined Claude Code 2.1.327 (design, Appendix B) and an MCP config of
// the spec's servers only, on every launch of the spec, a resumed one too.
func TestConfinedLaunch(t *testing.T) {
	a := newAdapter(t, `{"hataori": "/opt/hataori/bin/hataori"}`)
	home := t.TempDir() // no user's status line
	a.environ = func() []string { return []string{"PATH=/usr/bin:/bin", "HOME=" + home} }
	s := spec(t)
	s.Confine = &agent.Confinement{MCP: []agent.MCPServer{{Name: "hataori",
		Argv: []string{"/opt/hataori/bin/hataori", "mcp", "--scope", "C1", "--root", "/srv/root"}, Env: []string{"HATAORI_TOKEN=t0k=en"}}}}
	for _, resume := range []string{"", "00000000-0000-4000-8000-000000000001"} {
		s.ResumeID = resume
		ordinary := s
		ordinary.Confine = nil
		plain, err := a.Launch(ordinary)
		if err != nil {
			t.Fatal(err)
		}
		l, err := a.Launch(s)
		if err != nil {
			t.Fatal(err)
		}
		if !l.Confined || !l.NoMentions || !l.NoWriteTools {
			t.Fatalf("the launch does not say it is confined (%t), takes no input for a mention (%t) and leaves no tool that writes (%t)", l.Confined, l.NoMentions, l.NoWriteTools)
		}
		if plain.Confined || plain.NoMentions || plain.NoWriteTools {
			t.Fatal("an ordinary launch says it is confined, takes no input for a mention, or leaves no tool that writes")
		}
		mcp := filepath.Join(s.StateDir, MCPConfigFile)
		want := append(slices.Clone(plain.Argv), "--restricted", "--strict-mcp-config", "--mcp-config", mcp, "--tools", "Read,Glob,Grep",
			"--setting-sources", "user")
		got := slices.Clone(l.Argv)
		if resume == "" { // two new sessions have two random ids
			i := slices.Index(got, "--session-id")
			j := slices.Index(want, "--session-id")
			if i < 0 || i != j {
				t.Fatalf("argv %v", got)
			}
			got[i+1], want[j+1] = "", ""
		}
		if !slices.Equal(got, want) {
			t.Fatalf("argv\n%q\nwant\n%q", got, want)
		}
		if want := append(slices.Clone(plain.Env), ConfinedEnv...); !slices.Equal(l.Env, want) {
			t.Fatalf("env %q, want the ordinary one and %q", l.Env, ConfinedEnv)
		}
		var cfg map[string]map[string]struct {
			Type, Command string
			Args          []string
			Env           map[string]string
		}
		if err := json.Unmarshal(l.Files[MCPConfigFile], &cfg); err != nil {
			t.Fatal(err)
		}
		srv, ok := cfg["mcpServers"]["hataori"]
		if len(cfg) != 1 || len(cfg["mcpServers"]) != 1 || !ok || srv.Type != "stdio" || srv.Command != "/opt/hataori/bin/hataori" ||
			!slices.Equal(srv.Args, []string{"mcp", "--scope", "C1", "--root", "/srv/root"}) || len(srv.Env) != 1 || srv.Env["HATAORI_TOKEN"] != "t0k=en" {
			t.Fatalf("MCP config %s", l.Files[MCPConfigFile])
		}
		for name := range plain.Files {
			if name == InstructionsFile {
				if got, want := string(l.Files[name]), atSignNote+"\n\n"+s.Instructions; got != want {
					t.Fatalf("instructions %q, want the note on the at sign and the ordinary ones: %q", got, want)
				}
				continue
			}
			if !bytes.Equal(l.Files[name], plain.Files[name]) {
				t.Fatalf("file %s differs from the ordinary launch's", name)
			}
		}
	}

	// A confined spec with no servers still has none but its own: an
	// empty list, and --strict-mcp-config keeps the user's and the project's out.
	s.Confine = &agent.Confinement{}
	l, err := a.Launch(s)
	if err != nil || !l.Confined || string(l.Files[MCPConfigFile]) != "{\n  \"mcpServers\": {}\n}" {
		t.Fatalf("no servers: %v %s", err, l.Files[MCPConfigFile])
	}
	// One without instructions still gets the note on the at sign.
	s.Instructions = ""
	l, err = a.Launch(s)
	if err != nil || string(l.Files[InstructionsFile]) != atSignNote {
		t.Fatalf("no instructions: %v %q", err, l.Files[InstructionsFile])
	}
	if file, _ := argAfter(l.Argv, "--append-system-prompt-file"); file != filepath.Join(s.StateDir, InstructionsFile) {
		t.Fatalf("no instructions: the note is not given: %q", l.Argv)
	}
	// One that reports gets the session's socket and what it reports, beside
	// its own environment; the spec's own list stays as it is.
	own := append(make([]string, 0, 8), "HATAORI_TOKEN=t0k=en")
	s.Confine = &agent.Confinement{MCP: []agent.MCPServer{{Name: "hataori", Argv: []string{"x"}, Env: own, Reports: true}, {Name: "other", Argv: []string{"y"}}}}
	l, err = a.Launch(s)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct{ Env map[string]string }
	}
	if err := json.Unmarshal(l.Files[MCPConfigFile], &cfg); err != nil {
		t.Fatal(err)
	}
	_, socket, _ := strings.Cut(host.HookEnv(s.StateDir), "=")
	if got, want := cfg.MCPServers["hataori"].Env, map[string]string{"HATAORI_TOKEN": "t0k=en", host.HookEnvVar: socket,
		agent.MCPReportEnv: `{"hook_event_name":"hataori:mcp","server":"hataori"}`}; !maps.Equal(got, want) {
		t.Errorf("the reporting server's environment %q, want %q", got, want)
	}
	if got := cfg.MCPServers["other"].Env; len(got) != 0 {
		t.Errorf("the other server's environment %q", got)
	}
	for _, bad := range []agent.MCPServer{{Name: "", Argv: []string{"x"}}, {Name: "x"}, {Name: "x", Argv: []string{"x"}, Env: []string{"NOEQUALS"}}} {
		s.Confine = &agent.Confinement{MCP: []agent.MCPServer{bad}}
		if _, err := a.Launch(s); err == nil {
			t.Errorf("server %+v accepted", bad)
		}
	}
	s.Confine = &agent.Confinement{MCP: []agent.MCPServer{{Name: "a", Argv: []string{"x"}}, {Name: "a", Argv: []string{"y"}}}}
	if _, err := a.Launch(s); err == nil {
		t.Error("two servers of one name accepted")
	}
}

// The confined launch's flags are the set #268 checked against the real
// CLI, and its captures show each escape refused under them: a command, a
// web fetch, a subagent and a wake-up were not available, a write and a read
// outside the directory were refused, also after a resume with the same
// flags, and the call to the listed MCP server was allowed.
func TestConfinedFlagsAreTheCheckedOnes(t *testing.T) {
	script, err := os.ReadFile("testdata/confine.py")
	if err != nil {
		t.Fatal(err)
	}
	// Those were taken while the list held the tools that write, too; that a
	// shorter list confines as it does is TestConfinedHasNoBuiltInToolThatWrites.
	for _, line := range []string{`FILE_TOOLS = "Read,Write,Edit,Glob,Grep"`,
		`base = ["--restricted", "--strict-mcp-config", "--mcp-config", mcp_config(work)]`,
		`return base + ["--tools", FILE_TOOLS]`} {
		if !bytes.Contains(script, []byte(line)) {
			t.Fatalf("confine.py no longer has %s: the captures were taken with other flags than the launch's", line)
		}
	}
	type outcome struct {
		Variant      string
		Calls        []struct{ Event, Tool string }
		Written      *bool
		FilesOutside []string `json:"files_outside"`
	}
	read := func(name string) ([]string, outcome) {
		t.Helper()
		dir := filepath.Join("testdata", "2.1.327", "confine_files_"+name)
		var tools struct{ Tools []string }
		var o outcome
		for f, v := range map[string]any{"tools.json": &tools, "outcome.json": &o} {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, v); err != nil {
				t.Fatal(err)
			}
		}
		if o.Variant != "files" {
			t.Fatalf("%s was taken with the flags of %q", name, o.Variant)
		}
		return tools.Tools, o
	}
	// Not available: no tool for it in the list the model got, and no call.
	for name, absent := range map[string][]string{
		"shell":             {"Bash"},
		"webfetch":          {"WebFetch", "WebSearch"},
		"subagent":          {"Agent", "Task"},
		"wakeup":            {"CronCreate", "ScheduleWakeup", "Monitor"},
		"resume_same_flags": {"Bash"},
	} {
		tools, o := read(name)
		for _, tool := range tools {
			if slices.Contains(absent, tool) {
				t.Errorf("%s: the session had %s", name, tool)
			}
		}
		if len(o.Calls) != 0 {
			t.Errorf("%s: calls %v", name, o.Calls)
		}
	}
	// Refused: the tool was called, and no post-call hook followed it.
	for name, tool := range map[string]string{"write_outside": "Write", "read_outside": "Read"} {
		_, o := read(name)
		called := false
		for _, c := range o.Calls {
			if c.Tool == tool && c.Event == "PreToolUse" {
				called = true
			}
			if strings.HasPrefix(c.Event, "PostToolUse") {
				t.Errorf("%s: %s %s ran", name, c.Event, c.Tool)
			}
		}
		if !called {
			t.Errorf("%s: %s was not tried", name, tool)
		}
	}
	if _, o := read("write_outside"); o.Written == nil || *o.Written || len(o.FilesOutside) != 0 {
		t.Errorf("write_outside: written %v, outside %v", o.Written, o.FilesOutside)
	}
	// Allowed: the listed MCP server's tool ran.
	_, o := read("mcp")
	ran := false
	for _, c := range o.Calls {
		ran = ran || c.Event == "PostToolUse" && c.Tool == "mcp__sv__ping"
	}
	if !ran {
		t.Errorf("mcp: calls %v; want the server's tool to run", o.Calls)
	}
}

// What a confined session can write with, taken from the real CLI with the
// flags of the launch, at a start and at a resume: the tools of its requests
// are those that read and its MCP server's, and no call of a built-in tool
// that writes or starts something reached a tool or changed a file. With the
// list of before #868 the same calls of Edit and Write did.
func TestConfinedHasNoBuiltInToolThatWrites(t *testing.T) {
	script, err := os.ReadFile("testdata/writes.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`"writes_confined": "` + ConfinedTools + `",`,
		`"--restricted", "--strict-mcp-config", "--mcp-config", w.mcp, "--setting-sources", "user", "--tools", tools]`} {
		if !bytes.Contains(script, []byte(line)) {
			t.Fatalf("writes.py no longer has %s: the captures were taken with other flags than the launch's", line)
		}
	}
	type try struct {
		Case, Tool string
		Hooks      []string
		Results    []struct {
			Error bool
			Text  string
		}
		Changed []string
	}
	type phase struct {
		Tools [][]string
		Tries []try
	}
	read := func(name string) (flag string, phases map[string]phase) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", "2.1.332", name, "outcome.json"))
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Start, Resume phase
			Flag          string
		}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		return o.Flag, map[string]phase{"start": o.Start, "resume": o.Resume}
	}

	flag, phases := read("writes_confined")
	if flag != ConfinedTools {
		t.Fatalf("taken with --tools %s", flag)
	}
	reads := strings.Split(ConfinedTools, ",")
	slices.Sort(reads)
	// DeferredToolPlaceholder is no tool: the CLI knows no such one.
	want := append(slices.Clone(reads), "DeferredToolPlaceholder", "mcp__sv__ping")
	for name, p := range phases {
		if len(p.Tools) != 1 || !slices.Equal(p.Tools[0], want) {
			t.Errorf("%s: the tools of the requests are %v, want always %v", name, p.Tools, want)
		}
		if len(p.Tries) < 20 {
			t.Errorf("%s: %d calls were tried", name, len(p.Tries))
		}
		for _, c := range p.Tries {
			if len(c.Results) != 1 {
				t.Errorf("%s: %s was answered %d times", name, c.Case, len(c.Results))
				continue
			}
			if slices.Contains(reads, c.Tool) {
				if c.Results[0].Error || len(c.Changed) != 0 {
					t.Errorf("%s: %s: %+v", name, c.Case, c)
				}
				continue
			}
			if !c.Results[0].Error || !strings.Contains(c.Results[0].Text, "No such tool available: "+c.Tool) || len(c.Hooks) != 0 || len(c.Changed) != 0 {
				t.Errorf("%s: %s got to a tool: %+v", name, c.Case, c)
			}
		}
	}
	// Each of the tools that the hook into docs/ knows was tried.
	for tool := range writeTools {
		if !slices.ContainsFunc(phases["start"].Tries, func(c try) bool { return c.Tool == tool }) {
			t.Errorf("%s was not tried", tool)
		}
	}

	// The calls are such that they write where the tool is there.
	if flag, phases = read("writes_files"); flag != "Read,Write,Edit,Glob,Grep" {
		t.Fatalf("the contrast was taken with --tools %s", flag)
	}
	for name, p := range phases {
		wrote := map[string]bool{}
		for _, c := range p.Tries {
			wrote[c.Case] = len(c.Changed) == 1
		}
		if !wrote["edit"] || !wrote["write_old"] || !wrote["write_new"] {
			t.Errorf("%s: with the file tools the calls wrote %v", name, wrote)
		}
	}
}

// A confined session may write its own directory, so the status line of a
// project settings file there is not taken into its launch, where it would
// run unconfined at the next start; nor is the user's own, which runs in
// that directory. An ordinary session takes them, as before.
func TestConfinedLaunchRunsNoStatusLineButHataoris(t *testing.T) {
	a := newAdapter(t, `{"hataori": "/opt/hataori/bin/hataori"}`)
	home := t.TempDir()
	a.environ = func() []string { return []string{"HOME=" + home} }
	s := spec(t)
	write := func(dir, command string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"statusLine": map[string]any{"type": "command", "command": command}})
		if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(s.Dir, "touch planted")
	settings := func(s agent.Spec) string {
		t.Helper()
		l, err := a.Launch(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(l.Files[SettingsFile])
	}
	if got := settings(s); !strings.Contains(got, "touch planted") {
		t.Fatalf("an ordinary session lost its project's status line: %s", got)
	}
	confined := s
	confined.Confine = &agent.Confinement{}
	if got := settings(confined); strings.Contains(got, "touch planted") || strings.Contains(got, "--run") ||
		!strings.Contains(got, "statusline --last") {
		t.Fatalf("a confined session runs the status line of its own directory: %s", got)
	}
	// The user's own, which an ordinary session shows, as before.
	os.RemoveAll(filepath.Join(s.Dir, ".claude"))
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"statusLine": map[string]any{"type": "command", "command": "users-own-line"}})
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := settings(s); !strings.Contains(got, "users-own-line") {
		t.Fatalf("an ordinary session lost the user's status line: %s", got)
	}
	if got := settings(confined); strings.Contains(got, "users-own-line") || strings.Contains(got, "--run") {
		t.Fatalf("a confined session runs the user's status line in its own directory: %s", got)
	}
}
