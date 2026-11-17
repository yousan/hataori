//go:build linux || darwin

package workspace

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// gitCommand cancels git and all of its transport processes together.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}
