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
//	hataori version
//
// The root is --root, or $HATAORI_ROOT. Output is one JSON object per line,
// except for attach, whose output is the agent's terminal.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/control"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCtx(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runCtx(context.Background(), args, stdout, stderr)
}

const usage = "usage: hataori run|threads|thread NUMBER|events [--after N] [--follow]|stop NUMBER|attach NUMBER|close NUMBER|remove NUMBER|signin [--web] [--root DIR] | hataori connect [[user@]host] [--root DIR] [--hataori PATH] [--port N] | hataori mcp --scope SCOPE [--root DIR] [--output-max BYTES] | hataori version | hataori host <state-dir> | hataori hook [--wait] [--timeout DURATION] | hataori statusline --last FILE [--run COMMAND]"

func runCtx(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		if len(args) == 1 {
			fmt.Fprintln(stdout, version.String())
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
	case "hook":
		return host.HookMain(args[1:], os.Stdin, stdout, stderr, os.Getenv)
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
	case "run", "threads", "thread", "events", "stop", "attach", "close", "remove", "signin":
		return command(ctx, args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	return exitUsage
}

func command(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("HATAORI_ROOT"), "the instance root")
	after := fs.Uint64("after", 0, "events: print the events after this position")
	follow := fs.Bool("follow", false, "events: keep printing new events")
	web := fs.Bool("web", false, "signin: a link for the dashboard's web listener instead of its loopback listener")
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
	if name == "thread" || name == "stop" || name == "attach" || name == "close" || name == "remove" {
		want = 1
	}
	if want == 1 && len(pos) == 1 && !isNumber(pos[0]) {
		fmt.Fprintf(stderr, "hataori: %q is not a thread number\n", pos[0])
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
