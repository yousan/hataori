// Command hataori runs coding agents in parallel from chat.
//
// Usage:
//
//	hataori run [--root DIR]                    run the daemon of an instance
//	hataori threads [--root DIR]                one line per thread
//	hataori thread [--root DIR] KEY             the thread with KEY, or exit 3
//	hataori events [--root DIR] [--after N] [--follow]
//	hataori stop [--root DIR] KEY               interrupt the thread's running turn
//	hataori attach [--root DIR] KEY             this terminal on the agent's, until Ctrl-\
//	hataori signin [--root DIR]                 a single-use dashboard sign-in link
//	hataori version
//
// The root is --root, or $HATAORI_ROOT. Output is one JSON object per line,
// except for attach, whose output is the agent's terminal.
//
// Exit codes: 0 success; 1 an error, such as no daemon running for the
// root; 2 a usage error; 3 (thread) no thread of this instance has the key.
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
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
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

const usage = "usage: hataori run|threads|thread KEY|events [--after N] [--follow]|stop KEY|attach KEY|signin [--root DIR] | hataori version | hataori host <state-dir> | hataori hook [--wait] [--timeout DURATION] | hataori statusline --last FILE [--run COMMAND]"

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
	case "statusline":
		return claudecode.StatusLineMain(args[1:], os.Stdin, stdout, stderr, os.Getenv)
	case "run", "threads", "thread", "events", "stop", "attach", "signin":
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
	if name == "thread" || name == "stop" || name == "attach" {
		want = 1
	}
	if len(pos) != want || *root == "" || (name != "events" && (*after != 0 || *follow)) {
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
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the key %q\n", pos[0])
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
		link, err := c.SignInLink(ctx)
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
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the key %q\n", pos[0])
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
			fmt.Fprintf(stderr, "hataori: no thread of this instance has the key %q\n", pos[0])
			return exitNoThread
		}
		running := t.State == control.Running
		if err := c.Stop(ctx, t.ID); err != nil {
			return fail(err)
		}
		if err := enc.Encode(map[string]any{"scope": t.ID.Scope, "key": t.ID.Key, "running": running}); err != nil {
			return fail(err)
		}
	}
	return 0
}

type line struct {
	Scope    string    `json:"scope"`
	Key      string    `json:"key"`
	Number   int64     `json:"number"`
	State    string    `json:"state"`
	Title    string    `json:"title"`
	Repo     string    `json:"repo"`
	Updated  time.Time `json:"updated"`
	Agent    string    `json:"agent,omitempty"`
	Model    string    `json:"model,omitempty"`
	Effort   string    `json:"effort,omitempty"`
	Turns    int       `json:"turns"`
	Requests int       `json:"requests,omitempty"` // open questions, for thread
}

func threadLine(t control.ThreadInfo) line {
	return line{Scope: t.ID.Scope, Key: t.ID.Key, Number: t.Number, State: string(t.State), Title: t.Title,
		Repo: t.Repo, Updated: t.Updated, Agent: t.Agent, Model: t.Model, Effort: t.Effort, Turns: t.Turns}
}

// find returns the thread whose key is key, in any scope of the instance.
func find(ctx context.Context, c control.API, key string) (control.ThreadInfo, bool, error) {
	ts, err := c.Threads(ctx, control.Filter{Thread: control.ThreadID{Key: key}})
	if err != nil || len(ts) == 0 {
		return control.ThreadInfo{}, false, err
	}
	if len(ts) > 1 {
		return control.ThreadInfo{}, false, fmt.Errorf("%d threads have the key %q", len(ts), key)
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
