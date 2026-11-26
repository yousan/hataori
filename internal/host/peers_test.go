package host

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func init() {
	extraRoles["attach-peer"] = func(args []string) int {
		a, err := Attach(args[0])
		if err != nil {
			return 1
		}
		defer a.Close()
		if err := os.WriteFile(args[1], []byte("ready"), 0600); err != nil {
			return 2
		}
		var b [1]byte
		os.Stdin.Read(b[:])
		return 0
	}
}

// Real connections, including a client reached through an unrelated link
// outside the host binary's directory, are identified by the kernel.
func TestAttachmentPIDsAcrossConnections(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("echo"), Terminal: true, Env: []string{}})
	link := filepath.Join(t.TempDir(), "unrelated-client")
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(link, "role", "attach-peer", SocketPath(dir), ready)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	waitFor(t, "the renamed attach", func() bool { _, err := os.Stat(ready); return err == nil })
	one, err := Attach(SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := Attach(SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	want := []int{cmd.Process.Pid, os.Getpid(), os.Getpid()}
	slices.Sort(want)
	check := func(want []int) bool {
		// A fresh daemon connection can recover every existing attachment.
		c, err := Dial(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		got, err := c.AttachmentPIDs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return slices.Equal(got, want)
	}
	if !check(want) {
		t.Fatal("missing attached processes")
	}
	one.Close()
	want = []int{cmd.Process.Pid, os.Getpid()}
	slices.Sort(want)
	waitFor(t, "one of two connections from the same PID to detach", func() bool { return check(want) })
	in.Close()
	waitFor(t, "the renamed process to detach", func() bool { return check([]int{os.Getpid()}) })
}

func TestAttachmentPIDsRefuseUnknownIdentities(t *testing.T) {
	h := &host{attachments: map[*net.UnixConn]int{new(net.UnixConn): 0}}
	_, known, _ := h.attachmentPIDs()
	if known {
		t.Fatal("unknown PID considered complete")
	}
	for name, resp := range map[string]response{
		"older host":       {Attached: 1},
		"older empty host": {},
		"unknown peer":     {Attached: 1, AttachPIDs: []int{0}},
		"incomplete":       {Attached: 2, AttachPIDs: []int{42}, AttachPIDsKnown: true},
		"invalid PID":      {Attached: 1, AttachPIDs: []int{0}, AttachPIDsKnown: true},
	} {
		t.Run(name, func(t *testing.T) {
			// Force the short-socket fallback on every platform, including
			// Linux, where the test root would otherwise fit in a Unix path.
			dir := filepath.Join(t.TempDir(), strings.Repeat("long-", 24))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			socket := SocketPath(dir)
			socketDir := filepath.Dir(socket)
			if socketDir == dir {
				t.Fatal("fixture did not use the short-socket fallback")
			}
			if err := os.Mkdir(socketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(socketDir) }) // only our empty socket directory
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			defer func() { l.Close(); <-done }()
			go func() {
				defer close(done)
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var req request
				json.NewDecoder(c).Decode(&req)
				json.NewEncoder(c).Encode(resp)
			}()
			c, err := Dial(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.AttachmentPIDs(context.Background()); err == nil {
				t.Fatal("incomplete ownership accepted")
			}
		})
	}
}
