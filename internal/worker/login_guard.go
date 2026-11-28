package worker

import (
	"os"
	"os/exec"
	"syscall"
)

// LoginGuardMain runs only as the leader of a new process group. fd 3 is the
// resident's lifetime pipe; fd 4 is a private, one-byte clean-exit report. The
// guard kills its own live group even after the direct child exits. Detached
// descendants are outside that group; supported-CLI evidence must establish
// that they are not created. No saved PID becomes kill authority.
func LoginGuardMain(args []string) int {
	if syscall.Getpgrp() != os.Getpid() || len(args) == 0 {
		return 1
	}
	lifetime := os.NewFile(3, "resident")
	status := os.NewFile(4, "login-status")
	if lifetime == nil || status == nil {
		return 1
	}
	defer lifetime.Close()
	defer status.Close()
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	end := func() { syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL) }
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		end()
		return 1
	}
	go func() {
		var b [1]byte
		lifetime.Read(b[:])
		cmd.Process.Kill() // this still-owned direct child may have changed group
		end()
	}()
	code := byte(1)
	if cmd.Wait() == nil {
		code = 0
	}
	status.Write([]byte{code})
	status.Close()
	end()
	return 1 // reached only if killing our own live group failed
}
