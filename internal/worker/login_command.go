package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

type loginCommand struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lifetime *os.File
	status   *os.File
}

func startLoginCommand(ctx context.Context, launch agent.Launch, dir string, guard []string, output io.Writer) (*loginCommand, error) {
	if len(launch.Argv) == 0 || launch.Terminal || len(launch.Files) != 0 || len(guard) == 0 {
		return nil, errors.New("worker login: invalid local launch")
	}
	gr, gw, err := os.Pipe()
	if err != nil {
		return nil, errors.New("worker login: cannot open guard")
	}
	defer gr.Close()
	sr, sw, err := os.Pipe()
	if err != nil {
		gw.Close()
		return nil, errors.New("worker login: cannot open status")
	}
	defer sw.Close()
	argv := append(append([]string{}, guard...), launch.Argv...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	env := safeEnvironment(launch.Env)
	if env == nil {
		env = []string{}
	}
	cmd.Env, cmd.Dir = env, dir
	cmd.ExtraFiles = []*os.File{gr, sw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel, cmd.WaitDelay = gw.Close, time.Second
	cmd.Stdout, cmd.Stderr = output, output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		gw.Close()
		sr.Close()
		return nil, errors.New("worker login: cannot open input")
	}
	if cmd.Start() != nil {
		gw.Close()
		sr.Close()
		stdin.Close()
		return nil, errors.New("worker login: command did not start")
	}
	return &loginCommand{cmd: cmd, stdin: stdin, lifetime: gw, status: sr}, nil
}

func (c *loginCommand) wait(ctx context.Context) bool {
	defer c.lifetime.Close()
	defer c.stdin.Close()
	defer c.status.Close()
	err := c.cmd.Wait()
	// A correctly ending guard kills its own group. Only its separate clean
	// child-exit report can certify success; an arbitrary guard exit cannot.
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return false
	}
	c.status.SetReadDeadline(time.Now().Add(time.Second))
	b, err := io.ReadAll(io.LimitReader(c.status, 2))
	return err == nil && len(b) == 1 && b[0] == 0 && ctx.Err() == nil
}

type loginStatusOutput struct {
	remaining int
	cancel    context.CancelFunc
}

func (w *loginStatusOutput) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		w.cancel()
		return 0, errors.New("worker login: status output limit")
	}
	w.remaining -= len(b)
	return len(b), nil
}
