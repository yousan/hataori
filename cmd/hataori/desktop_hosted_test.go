package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/desktopstate"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/store"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/wire"
)

// This fixture is a private stdin/stdout peer of the hosted Electron proof,
// not a public command or service. It uses an isolated fake chat/agent root.
// The production launcher starts staged, separately compiled versioned test
// binaries and joins the old daemon before replacement. Host ownership is live
// socket capability evidence,
// never /proc, a saved PID, a process name or a process-group signal.
func TestDesktopHostedFixture(t *testing.T) {
	if os.Getenv("HATAORI_DESKTOP_HOSTED_FIXTURE") != "1" {
		t.Skip("private hosted fixture entry point")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("disposable standard GitHub runner required")
	}
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["drivers"] = map[string]any{remoteSurface: map[string]any{"socket": "DESKTOP_FIXTURE_SOCKET"}}
		cfg["scopes"].([]map[string]any)[0]["client"] = remoteSurface
	})
	// newChatRoot is a fresh fixture, not an adopted instance. Go's TempDir
	// mode is not the product's 0700 root contract on every hosted platform.
	if err := os.Chmod(r.root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Native discovery binds the canonical selected root. Use that same spelling
	// when the host computes its long socket path (macOS /var aliases /private).
	canonical, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		t.Fatal(err)
	}
	r.root = canonical
	svc := startService(t, r.driver) // scratch Unix fake-chat bridge, no provider
	config := filepath.Join(r.root, instance.ConfigFile)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, bytes.Replace(data, []byte(`"DESKTOP_FIXTURE_SOCKET"`), []byte(strconv.Quote(svc.sock)), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := desktopstate.Open(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	one := stageHostedFixture(t, state, version.Version, os.Args[0], os.Getenv("DESKTOP_TRANSPORT_BINARY"))
	nextVersion := os.Getenv("DESKTOP_PROOF_NEXT_VERSION")
	if nextVersion == version.Version || nextVersion == "" {
		t.Fatal("distinct compiled fixture version required")
	}
	two := stageHostedFixture(t, state, nextVersion, os.Getenv("DESKTOP_NEXT_FIXTURE_BINARY"), os.Getenv("DESKTOP_NEXT_TRANSPORT_BINARY"))
	if err := state.Register(context.Background(), one); err != nil {
		t.Fatal(err)
	}
	if err := state.SetIntent(context.Background(), desktopstate.Running); err != nil {
		t.Fatal(err)
	}
	var cancelLauncher context.CancelFunc
	var done chan error
	activeVersion := one.Version
	start := func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancelLauncher = cancel
		done = make(chan error, 1)
		go func() { done <- desktopLauncher(ctx, r.root, state) }()
	}
	ready := func() {
		var lastErr error
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, lastErr = nativehttp.Inspect(ctx, r.root, "hataori "+activeVersion)
			cancel()
			if lastErr == nil && svc.isAttached() {
				return
			}
		}
		t.Fatalf("private daemon readiness refused: %v", lastErr)
	}
	stop := func() {
		if cancelLauncher == nil {
			return
		}
		cancelLauncher()
		cancelLauncher = nil
		select {
		case err := <-done:
			if err != nil {
				t.Error("managed daemon did not exit successfully")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("owned daemon did not join")
		}
	}
	var id control.ThreadID
	t.Cleanup(func() {
		// Normal control operations end only this fixture's own fake session.
		// A failure cannot license a PID/tree kill; the disposable VM bounds it.
		if cancelLauncher != nil && id != 0 {
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
	start()
	ready()
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
	binding, err := nativehttp.Inspect(context.Background(), r.root, version.String())
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
		capability, err := host.ProbeInputCapability(context.Background(), runner.StateDir(r.root, int64(id)))
		if err != nil {
			t.Fatal(err)
		}
		return capability.Run
	}
	originalRun := hostRun()
	if err := state.CheckHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	emit("ready", map[string]any{"binding": binding, "thread": id})
	turnStarted := false
	var heldUntil time.Time
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024)
	for scanner.Scan() {
		switch scanner.Text() {
		case "turn":
			if turnStarted {
				t.Fatal("duplicate fixture turn")
			}
			turnStarted = true
			// The fake tool cannot finish 20s before its input was sent. Retain
			// that conservative bound across Select/join/readiness, too.
			heldUntil = time.Now().Add(20 * time.Second)
			r.driver.Inject(thread, fakesurface.Webhook(e2eHook), "tool wait 20s\nreply survived desktop daemon replacement")
			eventually(t, "held fake-agent turn", 10*time.Second, func() bool { return len(r.events(t, "tool_started", thread)) == 1 })
			emit("turn", nil)
		case "restart":
			if !turnStarted || len(r.events(t, "tool_ended", thread)) != 0 {
				t.Fatal("replacement was not during the held turn")
			}
			// Select a distinct immutable executable through the production
			// controller. Its owned child pipe and Wait must join before launch.
			if err := state.Select(context.Background(), two.Version, two.Binary); err != nil {
				t.Fatal(err)
			}
			activeVersion = two.Version
			ready()
			if !time.Now().Before(heldUntil) || len(r.events(t, "tool_ended", thread)) != 0 || len(r.events(t, "turn_ended", thread)) != 1 {
				t.Fatal("replacement became ready after the held turn could finish")
			}
			if err := state.CheckHosts(context.Background()); err != nil {
				t.Fatal(err)
			}
			next, err := nativehttp.Inspect(context.Background(), r.root, "hataori "+activeVersion)
			if err != nil || next.Instance != binding.Instance || hostRun() != originalRun {
				t.Fatal("replacement changed installation/host identity")
			}
			if err := state.VerifySelection(context.Background(), one); err != nil {
				t.Fatal("old referenced binary was lost", err)
			}
			emit("restarted", map[string]any{"binding": next})
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
		case "persistent-stop":
			if err := state.SetIntent(context.Background(), desktopstate.Stopped); err != nil {
				t.Fatal(err)
			}
			stop() // join only the launcher's owned child, leaving the host alive
			start()
			// A fresh production launcher must honor disk state across multiple
			// ticks; app/login startup cannot implicitly clear Stop.
			for range 3 {
				time.Sleep(500 * time.Millisecond)
				current, err := state.Read(context.Background())
				if err != nil || current.Intent != desktopstate.Stopped || hostRun() != originalRun {
					t.Fatal("launcher restart undid persistent Stop or replaced its host")
				}
				if _, err := nativehttp.Inspect(context.Background(), r.root, "hataori "+activeVersion); err == nil {
					t.Fatal("stopped daemon was restarted implicitly")
				}
			}
			if err := state.SetIntent(context.Background(), desktopstate.Running); err != nil {
				t.Fatal(err)
			}
			ready()
			emit("started", nil)
		default:
			t.Fatal("unexpected private fixture instruction")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

// Matching inert manifests describe the two real, separately compiled hosted
// fixture executables and transport helpers. Staging/selection/launch use the
// product implementation; these test-only binaries add the fake adapters.
func stageHostedFixture(t *testing.T, state *desktopstate.State, selected, binary, transport string) desktopstate.Selection {
	t.Helper()
	resources := t.TempDir()
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	manifest := desktopstate.Manifest{Format: 1, Version: selected, Commit: strings.TrimPrefix(version.Version, "0.0.0-dev."), Platform: "darwin", Arch: arch, Schema: store.SchemaVersion(), ControlProtocol: wire.Version, HostCapability: host.InputBatchVersion, Files: map[string]string{}}
	for name, source := range map[string]string{"hataori": binary, "hataori-desktop-transport": transport} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resources, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		manifest.Files[name] = hex.EncodeToString(digest[:])
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "desktop-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := state.Stage(context.Background(), resources, selected)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}
