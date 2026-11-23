// Command hataori runs coding agents in parallel from chat.
//
// Usage:
//
//	hataori run [--root DIR]                    run the daemon of an instance
//	hataori threads [--root DIR]                one line per thread
//	hataori thread [--root DIR] NUMBER          the thread with NUMBER (or #NUMBER), or exit 3
//	hataori events [--root DIR] [--after N] [--follow]
//	hataori stop [--root DIR] NUMBER            interrupt the thread's running turn
//	hataori attach [--root DIR] NUMBER          this terminal on the agent's, until Ctrl-\
//	hataori close [--root DIR] NUMBER           end the thread's agent and close it; the workspace stays
//	hataori remove [--root DIR] NUMBER          move a closed thread's workspace into trash, if nothing would be lost
//	hataori signin [--root DIR] [--web]         a single-use dashboard sign-in link, for the
//	                                            loopback listener or, with --web, the web listener
//	hataori connect [[user@]host] [--root DIR] [--hataori PATH] [--port N]
//	                                            on the person's machine: the dashboard of
//	                                            the instance on host, over SSH, until stopped
//	hataori mcp [--root DIR] --scope SCOPE [--output-max BYTES]
//	                                            the supervisor's MCP server on stdio;
//	                                            its token is $HATAORI_TOKEN
//	hataori take [--root DIR] WINDOW --scope SCOPE [--key KEY]
//	                                            make a thread of a window a person started, as
//	                                            HostTerminals' reported.window names it
//	hataori release [--root DIR] NUMBER         let a window taken in go: the thread goes asleep
//	hataori plugin [--root DIR]                 write the instance's Claude Code plugin under the
//	                                            root, and print the two commands that install it
//	hataori version
//
// The root is --root, or $HATAORI_ROOT. Output is one JSON object per line,
// except for attach, whose output is the agent's terminal, and plugin, which
// prints the commands for a person to run.
//
// Exit codes: 0 success; 1 an error, such as no daemon running for the
// root; 2 a usage error; 3 (thread, stop) no thread of this instance has
// the number.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/forge"
	"github.com/yousan/hataori/internal/forge/github"
	"github.com/yousan/hataori/internal/holder/tmux"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/discord"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/wire"
)

// Exit codes.
const (
	exitError    = 1
	exitUsage    = 2
	exitNoThread = 3
)

func init() {
	// Adapters and drivers are registered here, and only here.
	if err := forge.Register(github.Name, github.New); err != nil {
		panic(err)
	}
	if err := agent.Register(claudecode.Name, claudecode.New); err != nil {
		panic(err)
	}
	if err := agent.Register(codex.Name, codex.New); err != nil {
		panic(err)
	}
	if err := surface.Register(discord.Name, discord.New); err != nil {
		panic(err)
	}
}

func main() {
	ctx, stop := signalContext()
	defer stop()
	os.Exit(runCtx(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// signalContext is the context every hataori command runs in: SIGINT and
// SIGTERM end it, and do not end the process.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func run(args []string, stdout, stderr io.Writer) int {
	return runCtx(context.Background(), args, stdout, stderr)
}

const usage = "usage: hataori run|threads|thread NUMBER|events [--after N] [--follow]|stop NUMBER|attach NUMBER|close NUMBER|remove NUMBER|signin [--web]|take WINDOW --scope SCOPE [--key KEY]|release NUMBER|trusts|untrust ID [--root DIR] | hataori connect [[user@]host] [--root DIR] [--hataori PATH] [--port N] | hataori mcp --scope SCOPE [--root DIR] [--output-max BYTES] | hataori plugin [--root DIR] | hataori version | hataori host <state-dir> | hataori hook [--wait] [--timeout DURATION] | hataori hook --window --root DIR | hataori statusline --last FILE [--run COMMAND]"

func runCtx(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		if len(args) == 1 {
			fmt.Fprintln(stdout, version.Line())
			return 0
		}
	case "login-guard":
		return relay.GuardMain(args[1:])
	case "host":
		if len(args) == 2 {
			// Started by the daemon through host.Start; its output goes nowhere.
			if err := host.Run(args[1]); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
			return 0
		}
	case "exec":
		if len(args) == 2 {
			// Run by respawn-pane in a tmux window hataori started: it becomes
			// the agent, or says why it cannot.
			err := tmux.ExecMain(args[1])
			fmt.Fprintln(stderr, err)
			return exitError
		}
	case "pipe":
		if len(args) == 2 {
			// Run by pipe-pane -O in a tmux window hataori started, with the
			// pane's output on its standard input.
			if err := host.Pipe(args[1], os.Stdin); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
			return 0
		}
	case "hook":
		if slices.Contains(args[1:], "--window") {
			// The hook of the Claude Code plugin (hataori plugin). It prints
			// nothing, whatever happens, but the answer to a call of a
			// window taken in: the person's window shows it.
			main, ok := windowHooks[flagValue(args[1:], "--agent", claudecode.Name)]
			if !ok {
				fmt.Fprint(stderr, claudecode.WindowHookUsage)
				return host.HookUsage
			}
			return main(args[1:], os.Stdin, stdout, stderr, os.Getenv)
		}
		return host.HookMain(args[1:], os.Stdin, stdout, stderr, os.Getenv)
	case "plugin":
		return pluginMain(args[1:], stdout, stderr)
	case "mcp":
		return mcpMain(ctx, args[1:], os.Stdin, stdout, stderr, os.Getenv)
	case "connect":
		env, err := machineEnv()
		if err != nil {
			fmt.Fprintln(stderr, "hataori connect:", err)
			return exitError
		}
		return connectMain(ctx, args[1:], stderr, env)
	case "statusline":
		return claudecode.StatusLineMain(args[1:], os.Stdin, stdout, stderr, os.Getenv)
	case "run", "threads", "thread", "events", "stop", "attach", "close", "remove", "signin", "take", "release", "trusts", "untrust":
		return command(ctx, args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	return exitUsage
}

// windowHooks are the hooks of `hataori hook --window`, by the adapter
// named with --agent: what reads that agent's hook payloads.
var windowHooks = map[string]func(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int{
	claudecode.Name: func(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
		return claudecode.WindowHookMain(args, stdin, stderr, getenv)
	},
}

// flagValue is the value given to the flag name in args, as "name value"
// or "name=value"; def when it is not given.
func flagValue(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return def
}

func command(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("HATAORI_ROOT"), "the instance root")
	after := fs.Uint64("after", 0, "events: print the events after this position")
	follow := fs.Bool("follow", false, "events: keep printing new events")
	web := fs.Bool("web", false, "signin: a link for the dashboard's web listener instead of its loopback listener")
	scope := fs.String("scope", "", "take: the scope the window's thread is made in")
	key := fs.String("key", "", "take: makes the call safe to repeat; a new one each time by default")
	var pos []string // flags may come before or after the key
	for {
		if err := fs.Parse(args); err != nil {
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	want := 0
	if name == "thread" || name == "stop" || name == "attach" || name == "close" || name == "remove" || name == "take" || name == "release" || name == "untrust" {
		want = 1
	}
	if (name == "take") != (*scope != "") || name != "take" && *key != "" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if want == 1 && len(pos) == 1 && name != "take" && !isNumber(pos[0]) {
		what := "a thread number"
		if name == "untrust" {
			what = "the id of a trusted file"
		}
		fmt.Fprintf(stderr, "hataori: %q is not %s\n", pos[0], what)
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if len(pos) != want || *root == "" || (name != "events" && (*after != 0 || *follow)) || (name != "signin" && *web) {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "hataori:", err)
		return exitError
	}
	if name == "run" {
		if err := daemon(ctx, *root, stderr); err != nil {
			return fail(err)
		}
		return 0
	}

	c, err := wire.Dial(*root)
	if err != nil {
		return fail(fmt.Errorf("no hataori daemon runs for %s (%v)", *root, err))
	}
	defer c.Close()
	enc := json.NewEncoder(stdout)
	switch name {
	case "threads":
		ts, err := c.Threads(ctx, control.Filter{})
		if err != nil {
			return fail(err)
		}
		for _, t := range ts {
			if err := enc.Encode(threadLine(t)); err != nil {
				return fail(err)
			}
		}
		// A chat client that is not connected hears no new order. A
		// daemon before protocol 3.22 cannot tell, and says nothing.
		if cs, err := c.Clients(ctx); err == nil {
			for _, cl := range cs {
				if !cl.Connected {
					fmt.Fprintf(stderr, "hataori: chat client %s is not connected since %s: no message from it reaches hataori\n",
						cl.Client, cl.Since.Local().Format(time.RFC3339))
				}
			}
		}
	case "thread":
		t, ok, err := find(ctx, c, pos[0])
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the number %s\n", pos[0])
			return exitNoThread
		}
		d, err := c.Thread(ctx, t.ID, 0, 0)
		if err != nil {
			return fail(err)
		}
		l := threadLine(d.ThreadInfo)
		l.Requests = len(d.Requests)
		l.Cancelled = d.Cancelled
		if err := enc.Encode(l); err != nil {
			return fail(err)
		}
	case "events":
		return events(ctx, c, *after, *follow, enc, fail)
	case "signin":
		way := control.WayLocal
		if *web {
			way = control.WayWeb
		}
		link, err := c.SignInLink(ctx, way)
		if err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]string{"link": link}); err != nil {
			return fail(err)
		}
	case "attach":
		t, ok, err := find(ctx, c, pos[0])
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the number %s\n", pos[0])
			return exitNoThread
		}
		if err := attachTerminal(ctx, c, t, attachStdin, stdout, stderr); err != nil {
			return fail(err)
		}
	case "stop":
		t, ok, err := find(ctx, c, pos[0])
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the number %s\n", pos[0])
			return exitNoThread
		}
		running := t.State == control.Running
		if err := c.Stop(ctx, t.ID, 0); err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]any{"thread": t.ID, "scope": t.Scope, "running": running}); err != nil {
			return fail(err)
		}
	case "take":
		// Said before anything changes: from the window's next hook on, its
		// permission requests are the scope's to answer (design, section 4.8).
		fmt.Fprintf(stderr, "hataori take: from the window's next hook on, the rules of scope %q answer its permission requests; "+
			"today they are allowed automatically\n", *scope)
		k := *key
		if k == "" {
			k = "take-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		id, err := c.TakeIn(ctx, control.TakeInRequest{Window: pos[0], Scope: *scope, Key: k})
		if err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]any{"thread": id, "scope": *scope, "window": pos[0]}); err != nil {
			return fail(err)
		}
	case "release":
		t, ok, err := find(ctx, c, pos[0])
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the number %s\n", pos[0])
			return exitNoThread
		}
		if err := c.Release(ctx, t.ID); err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]any{"thread": t.ID, "scope": t.Scope, "state": control.Asleep}); err != nil {
			return fail(err)
		}
	case "trusts":
		ts, err := c.Trusts(ctx)
		if err != nil {
			return fail(err)
		}
		for _, t := range ts {
			if err := enc.Encode(t); err != nil {
				return fail(err)
			}
		}
	case "untrust":
		id, _ := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
		if err := c.Untrust(ctx, id); err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]any{"id": id, "trusted": false}); err != nil {
			return fail(err)
		}
	case "close", "remove":
		t, ok, err := find(ctx, c, pos[0])
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the number %s\n", pos[0])
			return exitNoThread
		}
		if name == "close" {
			err = c.CloseThread(ctx, t.ID)
		} else {
			err = c.RemoveThread(ctx, t.ID)
		}
		if err != nil {
			return fail(err)
		}
		state := control.Closed
		if name == "remove" {
			state = control.Removed
		}
		if err := enc.Encode(map[string]any{"thread": t.ID, "scope": t.Scope, "state": state}); err != nil {
			return fail(err)
		}
	}
	return 0
}

type line struct {
	Thread   control.ThreadID  `json:"thread"` // the thread's number
	Scope    string            `json:"scope"`
	Links    []control.LinkRef `json:"links"`
	State    string            `json:"state"`
	Title    string            `json:"title"`
	Repo     string            `json:"repo"`
	Updated  time.Time         `json:"updated"`
	Agent    string            `json:"agent,omitempty"`
	Model    string            `json:"model,omitempty"`
	Effort   string            `json:"effort,omitempty"`
	Turns    int               `json:"turns"`
	Requests int               `json:"requests,omitempty"` // open questions, for thread
	// Cancelled are the posts a plugin cancelled, for thread (#352).
	Cancelled []control.CancelledPost `json:"cancelled,omitempty"`
}

func threadLine(t control.ThreadInfo) line {
	return line{Thread: t.ID, Scope: t.Scope, Links: t.Links, State: string(t.State), Title: t.Title,
		Repo: t.Repo, Updated: t.Updated, Agent: t.Agent, Model: t.Model, Effort: t.Effort, Turns: t.Turns}
}

// isNumber reports whether arg is a thread number, with or without "#".
func isNumber(arg string) bool {
	n, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	return err == nil && n > 0
}

// find returns the thread whose number is arg, with or without "#", in
// any scope of the instance.
func find(ctx context.Context, c control.API, arg string) (control.ThreadInfo, bool, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil || n <= 0 {
		return control.ThreadInfo{}, false, fmt.Errorf("%q is not a thread number", arg)
	}
	ts, err := c.Threads(ctx, control.Filter{Thread: control.ThreadID(n)})
	if err != nil || len(ts) == 0 {
		return control.ThreadInfo{}, false, err
	}
	return ts[0], true, nil
}

// printEvent prints one event as the protocol's Event object
// (docs/protocol.md), one per line.
func printEvent(enc *json.Encoder, e control.Event) error { return enc.Encode(e) }

func events(ctx context.Context, c control.API, after uint64, follow bool, enc *json.Encoder, fail func(error) int) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := c.Events(ctx, control.Filter{}, after)
	if err != nil {
		return fail(err)
	}
	for e := range evs {
		if e.Kind == control.CaughtUp {
			if !follow {
				return 0
			}
			continue
		}
		if err := printEvent(enc, e); err != nil {
			return fail(err)
		}
	}
	if ctx.Err() != nil {
		return 0
	}
	return fail(errors.New("the daemon ended the event stream"))
}
