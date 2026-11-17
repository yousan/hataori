package fake

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/yousan/hataori/internal/agent"
)

// Main is the fake agent's program. Its flags are what Launch passes.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "login" {
		return loginMain(args[1:], stdin, stdout, stderr)
	}
	fs := flag.NewFlagSet("fakeagent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	terminal := fs.Bool("terminal", false, "")
	session := fs.String("session", "", "")
	resume := fs.String("resume", "", "")
	model := fs.String("model", "", "")
	effort := fs.String("effort", "", "")
	transcript := fs.String("transcript", "", "")
	hook := fs.String("hook", "", "the hataori binary, for `hataori hook`")
	neverStart := fs.Bool("never-start", false, "")
	conversations := fs.String("conversations", "", "a directory with one file per session started")
	configDir := fs.String("config", "", "where its login is kept; turns fail until it is logged in")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	p := &program{out: stdout, hook: *hook, terminal: *terminal, answers: map[string]chan string{}, configDir: *configDir}
	if *terminal {
		if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if old, err := term.MakeRaw(int(f.Fd())); err == nil {
				defer term.Restore(int(f.Fd()), old)
			}
		}
		t, err := os.OpenFile(*transcript, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer t.Close()
		p.transcript = t
	}
	inputs := make(chan string, 16)
	p.interrupts = make(chan struct{}, 1)
	if *terminal {
		go p.readTerminal(stdin, inputs)
	} else {
		go p.readPipes(stdin, inputs)
	}
	if *neverStart {
		fmt.Fprintln(stderr, "fake: never reporting the start") // for the end of the terminal log
		for range inputs {
		}
		return 0
	}
	id := *session
	if *resume != "" {
		id = *resume
	}
	if *conversations != "" {
		f := filepath.Join(*conversations, filepath.Base(id))
		if *resume != "" {
			if _, err := os.Stat(f); err != nil {
				p.report(map[string]any{"t": "cannot-resume", "session": id})
				return 1
			}
		} else if err := os.WriteFile(f, nil, 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	p.report(map[string]any{"t": "ready", "session": id, "model": *model, "effort": *effort, "resume": *resume, "transcript": *transcript})

	agentTurns := make(chan time.Duration, 4)
	for {
		select {
		case text, ok := <-inputs:
			if !ok {
				return 0
			}
			id := ""
			if rest, found := strings.CutPrefix(text, inputIDPrefix); found {
				id, text, _ = strings.Cut(rest, "\n")
			}
			if code, exit := p.turn("caller", id, text, agentTurns); exit {
				return code
			}
		case d := <-agentTurns:
			time.Sleep(d)
			p.turn("agent", "", "reply background done", agentTurns)
		}
	}
}

type program struct {
	mu         sync.Mutex
	out        io.Writer
	transcript *os.File
	hook       string
	terminal   bool
	asked      int
	answers    map[string]chan string
	interrupts chan struct{}
	configDir  string
}

// report sends one report: a line on stdout, or a hook call.
func (p *program) report(v map[string]any) string {
	b, _ := json.Marshal(v)
	if !p.terminal {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.out.Write(append(b, '\n'))
		return ""
	}
	args := []string{"hook"}
	if v["t"] == "ask" {
		args = append(args, "--wait", "--timeout", "1h")
	}
	cmd := exec.Command(p.hook, args...)
	cmd.Stdin = bytes.NewReader(b)
	out, _ := cmd.Output()
	return string(out)
}

// say writes text: to stdout on pipes, to the transcript on a terminal.
func (p *program) say(text string, final bool) {
	v := map[string]any{"t": "text", "text": text, "final": final}
	if !p.terminal {
		p.report(v)
		return
	}
	b, _ := json.Marshal(v)
	p.mu.Lock()
	p.transcript.Write(append(b, '\n'))
	p.mu.Unlock()
}

// inputIDPrefix starts the line before a script that names its input.
const inputIDPrefix = "@input "

// turn runs one script; input is the id the input named, if any. It
// returns an exit code and true when the script says to exit.
func (p *program) turn(by, input, script string, agentTurns chan time.Duration) (int, bool) {
	p.report(map[string]any{"t": "turn", "by": by, "input": input})
	end := map[string]any{"t": "end", "status": "completed", "reason": "", "background": 0}
	if !loggedIn(p.configDir) {
		end["status"], end["reason"] = "failed", string(agent.Login)
		p.report(end)
		return 0, false
	}
	final, replied := "done", false
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
		select {
		case <-p.interrupts:
			p.interrupted(end)
			return 0, false
		default:
		}
		switch cmd {
		case "reply":
			if replied {
				p.say(final, false)
			}
			final, replied = arg, true
		case "tool":
			name, dur, _ := strings.Cut(arg, " ")
			d, _ := time.ParseDuration(dur)
			p.report(map[string]any{"t": "tool", "id": "t-" + name, "name": name})
			if !p.runTool(d) {
				p.interrupted(end)
				return 0, false
			}
			p.report(map[string]any{"t": "tool", "id": "t-" + name, "name": name, "end": true})
		case "ask":
			p.asked++
			id := fmt.Sprintf("q%d", p.asked)
			answer, ok := p.ask(id, arg)
			if !ok {
				p.interrupted(end)
				return 0, false
			}
			final, replied = "answer: "+answer, true
		case "background":
			d, _ := time.ParseDuration(arg)
			end["background"] = 1
			agentTurns <- d
		case "fail":
			end["status"], end["reason"] = "failed", arg
		case "exit":
			return 3, true
		case "unknown":
			p.report(map[string]any{"t": "mystery"})
		case "subagent":
			// subagent ID [NAME [TOOL...]]: the tools run in it, one after another.
			f := strings.Fields(arg)
			id, name := f[0], ""
			if len(f) > 1 {
				name = f[1]
			}
			p.report(map[string]any{"t": "subagent", "id": id, "name": name})
			for i, tool := range f[min(2, len(f)):] {
				tid := fmt.Sprintf("%s-t%d", id, i+1)
				p.report(map[string]any{"t": "tool", "id": tid, "name": tool, "parent": id})
				p.report(map[string]any{"t": "tool", "id": tid, "name": tool, "parent": id, "end": true})
			}
			p.report(map[string]any{"t": "subagent", "id": id, "name": name, "end": true})
		case "file":
			p.report(map[string]any{"t": "file", "path": arg})
		}
	}
	p.say(final, true)
	end["answer"] = final
	p.report(end)
	return 0, false
}

// ask asks a question and waits for its answer, or for an interrupt.
func (p *program) ask(id, text string) (string, bool) {
	v := map[string]any{"t": "ask", "id": id, "text": text}
	if !p.terminal {
		ch := make(chan string, 1)
		p.mu.Lock()
		p.answers[id] = ch
		p.mu.Unlock()
		p.report(v)
		select {
		case a := <-ch:
			return a, true
		case <-p.interrupts:
			return "", false
		}
	}
	b, _ := json.Marshal(v)
	cmd := exec.Command(p.hook, "hook", "--wait", "--timeout", "1h")
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Start() != nil {
		return "", false
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return out.String(), true
	case <-p.interrupts:
		cmd.Process.Kill() // the host withdraws the call when its hook goes away
		<-done
		return "", false
	}
}

func (p *program) interrupted(end map[string]any) {
	if p.terminal {
		b, _ := json.Marshal(map[string]any{"t": "interrupted"})
		p.mu.Lock()
		p.transcript.Write(append(b, '\n'))
		p.mu.Unlock()
		return
	}
	end["status"] = "interrupted"
	p.report(end)
}

// runTool runs a child process for d, so that stopping a session has a
// process of the group to end. It returns false when interrupted.
func (p *program) runTool(d time.Duration) bool {
	cmd := exec.Command("sleep", fmt.Sprintf("%.3f", d.Seconds()))
	if err := cmd.Start(); err != nil {
		time.Sleep(d)
		return true
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-p.interrupts:
		cmd.Process.Kill()
		<-done
		return false
	}
}

func (p *program) readPipes(r io.Reader, inputs chan<- string) {
	defer close(inputs)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			Input     *string `json:"input"`
			Answer    *struct{ ID, Text string }
			Interrupt bool `json:"interrupt"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Input != nil:
			inputs <- *m.Input
		case m.Answer != nil:
			p.mu.Lock()
			ch := p.answers[m.Answer.ID]
			delete(p.answers, m.Answer.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- m.Answer.Text
			}
		case m.Interrupt:
			p.interrupt()
		}
	}
}

func (p *program) interrupt() {
	select {
	case p.interrupts <- struct{}{}:
	default:
	}
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// readTerminal reads bracketed pastes confirmed by Enter, and a lone
// Escape as an interrupt.
func (p *program) readTerminal(r io.Reader, inputs chan<- string) {
	defer close(inputs)
	var buf, pasted []byte
	in := make([]byte, 4096)
	for {
		n, err := r.Read(in)
		buf = append(buf, in[:n]...)
		for len(buf) > 0 {
			if bytes.HasPrefix(buf, []byte(pasteStart)) {
				end := bytes.Index(buf, []byte(pasteEnd))
				if end < 0 {
					break // the rest of the paste is still coming
				}
				pasted = append(pasted, buf[len(pasteStart):end]...)
				buf = buf[end+len(pasteEnd):]
				continue
			}
			if buf[0] == 0x1b {
				// A paste's start split by the read waits for the rest; a
				// lone Escape at the end of a read is the interrupt key.
				if len(buf) > 1 && len(buf) < len(pasteStart) && bytes.HasPrefix([]byte(pasteStart), buf) && err == nil {
					break
				}
				p.interrupt()
				buf = buf[1:]
				continue
			}
			if buf[0] == '\r' && len(pasted) > 0 {
				inputs <- string(pasted)
				pasted = nil
			}
			buf = buf[1:]
		}
		if err != nil {
			return
		}
	}
}
