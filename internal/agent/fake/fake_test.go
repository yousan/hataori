package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// The test binary is also the session host, `hataori hook` and the fake's
// program.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(host.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 2 && os.Args[1] == "role" {
		switch os.Args[2] {
		case "host":
			if host.Run(os.Args[3]) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "fake":
			os.Exit(Main(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
		}
		os.Exit(99)
	}
	os.Exit(m.Run())
}

func killGroups(dir string) {
	for _, f := range []string{host.PIDFile, host.HostPIDFile} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}
}

// session is the fake running under a real session host, read through its
// codec the way the runner will.
type session struct {
	t      *testing.T
	dir    string
	codec  agent.Codec
	client *host.Client
	src    agent.Source
	offs   map[string]int
	follow []string
	events []agent.Event
}

func adapter(t *testing.T, terminal bool) *Adapter {
	cfg, _ := json.Marshal(Config{Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0],
		Terminal: terminal, Env: []string{"PATH=" + os.Getenv("PATH")}})
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

func start(t *testing.T, a *Adapter, spec agent.Spec) *session {
	t.Helper()
	spec.StateDir = t.TempDir()
	t.Cleanup(func() { killGroups(spec.StateDir) })
	l, err := a.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, spec.StateDir,
		host.Spec{Argv: l.Argv, Env: l.Env, Terminal: l.Terminal}); err != nil {
		t.Fatal(err)
	}
	c, err := host.Dial(spec.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := &session{t: t, dir: spec.StateDir, codec: a.NewCodec(spec), client: c, src: agent.Output, offs: map[string]int{}}
	if a.c.Terminal {
		s.src = agent.Hook
	}
	return s
}

// do carries out a codec's actions.
func (s *session) do(a agent.Actions) {
	s.events = append(s.events, a.Events...)
	s.follow = append(s.follow, a.Follow...)
	for _, w := range a.Write {
		if err := s.client.Input(w); err != nil {
			s.t.Fatal(err)
		}
	}
	for _, r := range a.Reply {
		if err := s.client.Answer(r.Call, r.Body); err != nil {
			s.t.Fatal(err)
		}
	}
}

// poll decodes the complete lines that are new in the log and in the
// followed files.
func (s *session) poll() {
	files := append([]string{filepath.Join(s.dir, host.EventsFile)}, s.follow...)
	for i, f := range files {
		b, _ := os.ReadFile(f)
		end := bytes.LastIndexByte(b, '\n') + 1
		for _, l := range bytes.Split(b[s.offs[f]:end], []byte("\n")) {
			if len(l) == 0 {
				continue
			}
			src := s.src
			if i > 0 {
				src = agent.File
			}
			s.do(s.codec.Decode(src, l))
		}
		s.offs[f] = end
	}
}

func (s *session) until(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.poll()
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s; events: %s", what, describe(s.events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *session) has(k agent.Kind) func() bool {
	return func() bool { return s.count(k) > 0 }
}

func (s *session) count(k agent.Kind) int {
	n := 0
	for _, e := range s.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (s *session) send(text string) {
	a, err := s.codec.Input(agent.Input{Text: text})
	if err != nil {
		s.t.Fatal(err)
	}
	s.do(a)
}

var names = map[agent.Kind]string{agent.Unknown: "unknown", agent.SessionReady: "ready", agent.TurnStarted: "turn",
	agent.Text: "text", agent.ToolStarted: "tool+", agent.ToolEnded: "tool-", agent.SubagentStarted: "sub+",
	agent.SubagentEnded: "sub-", agent.Request: "request", agent.FileOut: "file", agent.TurnEnded: "end"}

func describe(evs []agent.Event) string {
	var b []string
	for _, e := range evs {
		s := names[e.Kind]
		switch e.Kind {
		case agent.TurnStarted:
			s += [...]string{"(caller)", "(agent)", "(terminal)"}[e.StartedBy]
		case agent.Text:
			s += fmt.Sprintf("(%s,%v)", e.Text, e.Final)
		case agent.TurnEnded:
			s += fmt.Sprintf("(%d,%s,bg=%d)", e.Status, e.Reason, e.Background)
		case agent.ToolStarted, agent.ToolEnded, agent.SubagentStarted, agent.SubagentEnded:
			s += "(" + e.ID
			if e.Tool != "" && e.Kind == agent.SubagentStarted {
				s += " " + e.Tool
			}
			if e.Parent != "" {
				s += " in " + e.Parent
			}
			s += ")"
		case agent.FileOut:
			s += "(" + strings.Join(append([]string{e.File}, e.More...), ",")
			if e.Caption != "" {
				s += ": " + e.Caption
			}
			s += ")"
		}
		b = append(b, s)
	}
	return strings.Join(b, " ")
}

// after returns the events after the session's start.
func (s *session) after() string {
	for i, e := range s.events {
		if e.Kind == agent.SessionReady {
			return describe(s.events[i+1:])
		}
	}
	return describe(s.events)
}

func forms(t *testing.T, f func(t *testing.T, terminal bool)) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) { f(t, terminal) })
	}
}

func TestScripts(t *testing.T) {
	cases := []struct {
		name, script, want string
		answer             string
	}{
		{"reply", "reply hello", "turn(caller) text(hello,true) end(0,,bg=0)", ""},
		{"two replies", "reply done\nreply second", "turn(caller) text(done,false) text(second,true) end(0,,bg=0)", ""},
		{"tool", "tool grep 100ms", "turn(caller) tool+(t-grep) tool-(t-grep) text(done,true) end(0,,bg=0)", ""},
		{"ask", "ask which colour?", "turn(caller) request text(answer: blue,true) end(0,,bg=0)", "blue"},
		{"background", "background 100ms\nreply started",
			"turn(caller) text(started,true) end(0,,bg=1) turn(agent) text(background done,true) end(0,,bg=0)", ""},
		{"fail", "fail limit", "turn(caller) text(done,true) end(2,limit,bg=0)", ""},
		{"unknown", "unknown", "turn(caller) unknown text(done,true) end(0,,bg=0)", ""},
		{"subagent", "subagent s1", "turn(caller) sub+(s1) sub-(s1) text(done,true) end(0,,bg=0)", ""},
		{"subagent with tools", "subagent s1 explorer Read Grep",
			"turn(caller) sub+(s1 explorer) tool+(s1-t1 in s1) tool-(s1-t1 in s1) tool+(s1-t2 in s1) tool-(s1-t2 in s1) sub-(s1) text(done,true) end(0,,bg=0)", ""},
		{"whoami", "whoami", "turn(caller) text(model  effort ,true) end(0,,bg=0)", ""},
		{"file", "file /tmp/report.txt", "turn(caller) file(/tmp/report.txt) text(done,true) end(0,,bg=0)", ""},
		{"files with a caption", "file /tmp/a.txt /tmp/b.txt -- two files",
			"turn(caller) file(/tmp/a.txt,/tmp/b.txt: two files) text(done,true) end(0,,bg=0)", ""},
	}
	forms(t, func(t *testing.T, terminal bool) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := start(t, adapter(t, terminal), agent.Spec{})
				s.until("ready", s.has(agent.SessionReady))
				s.send(tc.script)
				if tc.answer != "" {
					s.until("the question", s.has(agent.Request))
					for _, e := range s.events {
						if e.Kind == agent.Request {
							a, err := s.codec.Answer(agent.Answer{Request: e.Request.ID, Text: tc.answer})
							if err != nil {
								t.Fatal(err)
							}
							s.do(a)
						}
					}
				}
				ends := strings.Count(tc.want, "end(")
				s.until("the end", func() bool { return s.count(agent.TurnEnded) == ends })
				got := s.after()
				if terminal {
					// Text comes from the transcript and the rest from hook
					// calls: each is in order, the two are not ordered.
					got, tc.want = bySource(got), bySource(tc.want)
				}
				if got != tc.want {
					t.Fatalf("got  %s\nwant %s", got, tc.want)
				}
			})
		}
	})
}

func TestExitWithoutEndingTheTurn(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		s := start(t, adapter(t, terminal), agent.Spec{})
		s.until("ready", s.has(agent.SessionReady))
		s.send("reply never said\nexit")
		s.until("the exit", func() bool { _, ok, _ := host.ReadStatus(s.dir); return ok })
		s.poll()
		st, _, _ := host.ReadStatus(s.dir)
		if st.Code != 3 || s.count(agent.TurnEnded) != 0 {
			t.Fatalf("status %+v, events %s", st, s.after())
		}
	})
}

func TestNeverStarts(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		s := start(t, adapter(t, terminal), agent.Spec{Instructions: "never-start"})
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			s.poll()
			time.Sleep(20 * time.Millisecond)
		}
		if len(s.events) != 0 {
			t.Fatalf("a fake that never starts reported %s", describe(s.events))
		}
	})
}

func TestReportsModelEffortAndResume(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		s := start(t, adapter(t, terminal), agent.Spec{Model: "m1", Effort: "high", ResumeID: "fake-resumed"})
		s.until("ready", s.has(agent.SessionReady))
		e := s.events[0]
		if e.Model != "m1" || e.Effort != "high" || e.SessionID != "fake-resumed" {
			t.Fatalf("ready: %+v", e)
		}
	})
}

func TestTerminalOutputNeverReachesTheCodec(t *testing.T) {
	c := adapter(t, true).NewCodec(agent.Spec{})
	defer func() {
		if recover() == nil {
			t.Fatal("the terminal form's codec accepted terminal output")
		}
	}()
	c.Decode(agent.Output, []byte(`{"t":"text","text":"from the screen"}`))
}

func TestNotInTheCommand(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/yousan/hataori/cmd/hataori").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	if bytes.Contains(out, []byte("internal/agent/fake")) {
		t.Fatal("cmd/hataori depends on the fake agent")
	}
}

var token = regexp.MustCompile(`[a-z+-]+(\([^)]*\))?`)

// bySource puts the text events after the others, keeping each in order.
func bySource(desc string) string {
	var texts, rest []string
	for _, f := range token.FindAllString(desc, -1) {
		if strings.HasPrefix(f, "text(") {
			texts = append(texts, f)
		} else {
			rest = append(rest, f)
		}
	}
	return strings.Join(append(rest, texts...), " ")
}

// An interrupt ends a running tool and an open question, in both forms.
func TestInterrupt(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		for _, script := range []string{"tool long 30s", "ask anything?"} {
			s := start(t, adapter(t, terminal), agent.Spec{})
			s.until("ready", s.has(agent.SessionReady))
			s.send(script)
			if strings.HasPrefix(script, "tool") {
				s.until("the tool", s.has(agent.ToolStarted))
			} else {
				s.until("the question", s.has(agent.Request))
			}
			s.do(s.codec.Interrupt())
			s.until("the interrupted end", s.has(agent.TurnEnded))
			for _, e := range s.events {
				if e.Kind == agent.TurnEnded && e.Status != agent.Interrupted {
					t.Fatalf("%s: %s", script, s.after())
				}
			}
		}
	})
}

// A question can name choices: they reach the codec's request in order,
// and an answer of a choice is the reply. A question without " -- " has
// none.
func TestAskWithChoices(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		for _, tc := range []struct {
			script, question string
			choices          []string
			answer           agent.Answer
			reply            string
		}{
			{"ask Which colour? -- red | green | blue", "Which colour?", []string{"red", "green", "blue"}, agent.Answer{Choices: []string{"green"}}, "answer: green"},
			{"ask Which colour?", "Which colour?", nil, agent.Answer{Text: "teal"}, "answer: teal"},
		} {
			s := start(t, adapter(t, terminal), agent.Spec{})
			s.until("ready", s.has(agent.SessionReady))
			s.send(tc.script)
			s.until("the question", s.has(agent.Request))
			var r *agent.RequestInfo
			for _, e := range s.events {
				if e.Kind == agent.Request {
					r = e.Request
				}
			}
			if r.Text != tc.question || !slices.Equal(r.Choices, tc.choices) {
				t.Fatalf("%q: request %q with choices %q, want %q with %q", tc.script, r.Text, r.Choices, tc.question, tc.choices)
			}
			tc.answer.Request = r.ID
			a, err := s.codec.Answer(tc.answer)
			if err != nil {
				t.Fatal(err)
			}
			s.do(a)
			s.until("the end", s.has(agent.TurnEnded))
			var final string
			for _, e := range s.events {
				if e.Kind == agent.Text && e.Final {
					final = e.Text
				}
			}
			if final != tc.reply {
				t.Fatalf("%q: replied %q, want %q", tc.script, final, tc.reply)
			}
		}
	})
}
