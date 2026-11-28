package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/desktopstate"
	"github.com/yousan/hataori/internal/version"
)

func desktopMain(ctx context.Context, args []string, input io.ReadCloser, output, stderr io.Writer) int {
	if len(args) == 0 {
		return exitUsage
	}
	if args[0] == "metadata" && len(args) == 1 {
		if json.NewEncoder(output).Encode(desktopMetadata()) != nil {
			return exitError
		}
		return 0
	}
	switch args[0] {
	case "child", "launcher", "register", "select", "start", "stop", "status", "setup", "stage", "login-install", "login-info":
	default:
		return exitUsage
	}
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the explicitly selected private instance root")
	binary := fs.String("binary", "", "the verified immutable versioned daemon")
	selected := fs.String("version", "", "the verified bundled version")
	resources := fs.String("resources", "", "the selected matching application's resources")
	loginDir := fs.String("login-directory", "", "the user's LaunchAgents directory")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || *root == "" {
		return exitUsage
	}
	if args[0] != "register" && args[0] != "select" && (*binary != "" || *selected != "") {
		return exitUsage
	}
	if args[0] != "stage" && *resources != "" || args[0] == "stage" && *resources == "" {
		return exitUsage
	}
	if args[0] != "login-install" && *loginDir != "" || args[0] == "login-install" && *loginDir == "" {
		return exitUsage
	}
	var err error
	if args[0] == "child" {
		if file, ok := input.(*os.File); ok {
			info, statErr := file.Stat()
			if statErr != nil || info.Mode()&os.ModeNamedPipe == 0 {
				fmt.Fprintln(stderr, "Managed daemon requires its launcher's lifetime pipe")
				return exitError
			}
		}
		err = desktopChild(ctx, *root, input, func(ctx context.Context, root string) int {
			return command(ctx, "run", []string{"--root", root, "--native"}, output, stderr)
		})
	} else if args[0] == "setup" {
		err = desktopSetupMain(ctx, *root, input, output)
	} else {
		var s *desktopstate.State
		s, err = desktopstate.Open(ctx, *root)
		if err == nil {
			defer s.Close()
			switch args[0] {
			case "login-info", "login-install":
				var reg desktopstate.Registration
				if args[0] == "login-info" {
					reg, err = s.ReadLogin(ctx)
				} else {
					env := map[string]string{}
					for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL"} {
						if value, ok := os.LookupEnv(key); ok {
							env[key] = value
						}
					}
					reg, err = s.InstallLogin(ctx, *loginDir, env)
				}
				if err == nil {
					err = json.NewEncoder(output).Encode(reg)
				}
			case "stage":
				var d desktopstate.Selection
				d, err = s.Stage(ctx, *resources, version.Version)
				if err == nil {
					err = json.NewEncoder(output).Encode(d)
				}
			case "register":
				err = s.Register(ctx, desktopstate.Selection{Intent: desktopstate.Stopped, Version: *selected, Binary: *binary})
			case "select":
				err = s.Select(ctx, *selected, *binary)
			case "start":
				err = s.SetIntent(ctx, desktopstate.Running)
			case "stop":
				err = s.SetIntent(ctx, desktopstate.Stopped)
			case "status":
				var d desktopstate.Selection
				d, err = s.Read(ctx)
				if err == nil {
					err = json.NewEncoder(output).Encode(d)
				}
			case "launcher":
				err = desktopLauncher(ctx, *root, s)
			default:
				return exitUsage
			}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "Desktop operation refused:", err)
		return exitError
	}
	return 0
}

// A parent lifetime pipe binds the managed daemon to this specific launcher.
// Launcher crash/replacement ends only the daemon context, never detached
// session hosts. No saved PID, /proc lookup, signal or process-tree kill is used.
func desktopChild(parent context.Context, root string, input io.ReadCloser, run func(context.Context, string) int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var unexpected [1]byte
		input.Read(unexpected[:]) // EOF or any unexpected byte ends the lease
		cancel()
	}()
	code := run(ctx, root)
	input.Close()
	<-done
	if code != 0 {
		return errors.New("managed daemon exited unsuccessfully")
	}
	return nil
}

type desktopOwnedChild struct {
	input io.WriteCloser
	done  chan struct{}
	once  sync.Once
	err   error
}

func (c *desktopOwnedChild) Stop() error {
	c.once.Do(func() { c.err = c.input.Close() })
	return c.err
}
func (c *desktopOwnedChild) Done() <-chan struct{} { return c.done }

func desktopLauncher(ctx context.Context, root string, s *desktopstate.State) (err error) {
	lease, err := s.LockLauncher()
	if err != nil {
		return err
	}
	defer lease.Close()
	c := &desktopstate.Controller{Read: s.Read, Launch: func(_ context.Context, d desktopstate.Selection) (desktopstate.Child, error) {
		if err := s.VerifySelection(ctx, d); err != nil {
			return nil, err
		}
		cmd := exec.Command(d.Binary, "desktop", "child", "--root", root)
		// Inherit the explicit LaunchAgent GUI environment, including absolute
		// configured agent paths. Never query a login shell or copy credentials.
		cmd.Env = os.Environ()
		input, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		child := &desktopOwnedChild{input: input, done: make(chan struct{})}
		if err := cmd.Start(); err != nil {
			input.Close()
			return nil, err
		}
		go func() { cmd.Wait(); close(child.done) }()
		return child, nil
	}}
	defer func() {
		joinCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = errors.Join(err, c.Close(joinCtx))
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := c.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
