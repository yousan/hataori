package relay

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// GuardMain must lead its own process group. Its fd 3 is a pipe whose write
// end only the daemon holds; when a read returns, the daemon is gone and the
// guard kills the whole group. Otherwise it ends as its child ended.
func GuardMain(args []string) int {
	if syscall.Getpgrp() != os.Getpid() {
		fmt.Fprintln(os.Stderr, "login guard: not a process group leader")
		return 1
	}
	if len(args) == 0 {
		return 1
	}
	pipe := os.NewFile(3, "daemon")
	if pipe == nil {
		return 1
	}
	defer pipe.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go func() {
		var b [1]byte
		pipe.Read(b[:])
		syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL)
	}()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			sig := status.Signal()
			signal.Reset(sig)
			syscall.Kill(os.Getpid(), sig)
			time.Sleep(time.Second)
			return 128 + int(sig)
		}
		return exit.ExitCode()
	}
	return 1
}
