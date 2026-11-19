package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/yousan/hataori/internal/connect"
)

// connectEnv is what `hataori connect` takes from the person's machine:
// the ssh and the browser's launcher on PATH, the configuration directory
// where hosts are remembered, and a directory only the person can open
// for the control sockets. Tests give their own.
type connectEnv struct {
	SSH, Open         string
	StateFile, RunDir string
}

// machineEnv is connectEnv of this machine.
func machineEnv() (connectEnv, error) {
	var e connectEnv
	e.SSH, _ = exec.LookPath("ssh")
	launcher := "xdg-open"
	if runtime.GOOS == "darwin" {
		launcher = "open"
	}
	e.Open, _ = exec.LookPath(launcher)
	cfg, err := os.UserConfigDir()
	if err != nil {
		return e, fmt.Errorf("no configuration directory to remember hosts in: %w", err)
	}
	e.StateFile = filepath.Join(cfg, "hataori", "connect.json")
	// Unix socket paths are short, so the run directory is near the root:
	// the user's runtime directory, else one of their own in the system's
	// temporary directory (per user on macOS already).
	own := "hataori-" + strconv.Itoa(os.Getuid())
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && runtime.GOOS != "darwin" {
		e.RunDir = filepath.Join(d, "hataori")
	} else {
		e.RunDir = filepath.Join(os.TempDir(), own)
	}
	// Too long for the sockets (a long $TMPDIR): /tmp, in a directory that
	// connect checks is the person's alone.
	if len(e.RunDir) > 60 {
		e.RunDir = filepath.Join("/tmp", own)
	}
	return e, nil
}

const connectUsage = "usage: hataori connect [[user@]host] [--root DIR] [--hataori PATH] [--port N]"

// connectMain is `hataori connect`: the dashboard of an instance on
// another machine, over SSH, until ctx ends (design, section 7.2).
func connectMain(ctx context.Context, args []string, stderr io.Writer, env connectEnv) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the instance root on the host; the one remembered for the host when not given")
	bin := fs.String("hataori", "", "the hataori binary on the host, for a non-interactive SSH session whose PATH does not have it")
	port := fs.Int("port", 0, "the local port; the one remembered, else the dashboard's own number, else any free one")
	var pos []string // flags may come before or after the host
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
	if len(pos) > 1 || *port < 0 || *port > 65535 {
		fmt.Fprintln(stderr, connectUsage)
		return exitUsage
	}
	target := ""
	if len(pos) == 1 {
		target = pos[0]
	}
	err := connect.Run(ctx, connect.Options{Target: target, Root: *root, Hataori: *bin, Port: *port,
		SSH: env.SSH, Open: env.Open, StateFile: env.StateFile, RunDir: env.RunDir, Log: stderr})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "hataori connect:", err)
		return exitError
	}
	return 0
}
