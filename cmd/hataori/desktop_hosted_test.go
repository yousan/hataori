package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/desktopstate"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/runner"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/wire"
)

// This fixture is a private stdin/stdout peer of the hosted Electron proof,
// not a public command or service. It uses an isolated fake chat/agent root.
// The lifetime-pipe transition runs the real daemon and joins it before the
// replacement starts. Host ownership is live socket capability evidence,
// never /proc, a saved PID, a process name or a process-group signal.
func TestDesktopHostedFixture(t *testing.T) {
	if os.Getenv("HATAORI_DESKTOP_HOSTED_FIXTURE") != "1" {
		t.Skip("private hosted fixture entry point")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("disposable standard GitHub runner required")
	}
	r := newChatRoot(t, nil)
	var input *io.PipeWriter
	var done chan error
	start := func() {
		reader, writer := io.Pipe()
		input = writer
		done = make(chan error, 1)
		go func() {
			done <- desktopChild(context.Background(), r.root, reader, func(ctx context.Context, root string) int {
				return runCtx(ctx, []string{"run", "--root", root, "--native"}, io.Discard, io.Discard)
			})
		}()
		eventually(t, "private daemon ready", 20*time.Second, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := nativehttp.Inspect(ctx, r.root, version.Version)
			return err == nil && r.driver.Connected()
		})
	}
	stop := func() {
		if input == nil {
			return
		}
		input.Close()
		input = nil
		select {
		case err := <-done:
			if err != nil {
				t.Error("managed daemon did not exit successfully")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("owned daemon did not join")
		}
	}
	start()
	var id control.ThreadID
	t.Cleanup(func() {
		// Normal control operations end only this fixture's own fake session.
		// A failure cannot license a PID/tree kill; the disposable VM bounds it.
		if input != nil && id != 0 {
			if c, err := wire.Dial(r.root); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				c.Stop(ctx, id, 0)
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					if c.CloseThread(ctx, id) == nil {
						break
					}
				}
				cancel()
				c.Close()
			}
		}
		stop()
	})
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply desktop fixture ready")
	thread := r.thread(t, 0)
	r.posted(t, thread, "desktop fixture ready")
	eventually(t, "initial turn completion", 10*time.Second, func() bool { return len(r.events(t, "turn_ended", thread)) == 1 })
	n, err := strconv.ParseInt(numberOf(t, r.root, thread), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	id = control.ThreadID(n)
	binding, err := nativehttp.Inspect(context.Background(), r.root, version.Version)
	if err != nil {
		t.Fatal(err)
	}
	emit := func(event string, extra map[string]any) {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["event"] = event
		data, _ := json.Marshal(extra)
		fmt.Fprintln(os.Stdout, "DESKTOP_FIXTURE "+string(data))
	}
	hostRun := func() string {
		c, err := host.Dial(runner.StateDir(r.root, int64(id)))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		capability, err := c.InputCapability()
		if err != nil {
			t.Fatal(err)
		}
		return capability.Run
	}
	originalRun := hostRun()
	state, err := desktopstate.Open(context.Background(), binding.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.CheckHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	emit("ready", map[string]any{"binding": binding, "thread": id})
	turnStarted := false
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024)
	for scanner.Scan() {
		switch scanner.Text() {
		case "turn":
			if turnStarted {
				t.Fatal("duplicate fixture turn")
			}
			turnStarted = true
			r.driver.Inject(thread, fakesurface.Webhook(e2eHook), "tool wait 20s\nreply survived desktop daemon replacement")
			eventually(t, "held fake-agent turn", 10*time.Second, func() bool { return len(r.events(t, "tool_started", thread)) == 1 })
			emit("turn", nil)
		case "restart":
			if !turnStarted || len(r.events(t, "tool_ended", thread)) != 0 {
				t.Fatal("replacement was not during the held turn")
			}
			stop()
			if got := hostRun(); got != originalRun {
				t.Fatal("daemon stop changed the live host run")
			}
			start()
			if err := state.CheckHosts(context.Background()); err != nil {
				t.Fatal(err)
			}
			next, err := nativehttp.Inspect(context.Background(), r.root, version.Version)
			if err != nil || next.Instance != binding.Instance || hostRun() != originalRun {
				t.Fatal("replacement changed installation/host identity")
			}
			emit("restarted", nil)
		case "finish":
			r.posted(t, thread, "survived desktop daemon replacement")
			eventually(t, "replacement turn completion", 30*time.Second, func() bool { return len(r.events(t, "turn_ended", thread)) == 2 })
			if hostRun() != originalRun {
				t.Fatal("replacement restarted the session host")
			}
			count := 0
			for _, call := range r.driver.Calls(thread) {
				if call.Op == fakesurface.OpPost && call.Message.Text == "survived desktop daemon replacement" {
					count++
				}
			}
			if count != 1 {
				t.Fatal("replacement replayed or lost the answer")
			}
			emit("finished", nil)
		default:
			t.Fatal("unexpected private fixture instruction")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
