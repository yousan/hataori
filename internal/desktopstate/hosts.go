package desktopstate

import (
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/nativehttp"
)

// CheckHosts refuses replacement when an existing private session host cannot
// prove the manifest's current input capability. The permanent host lock is a
// liveness check, not permission to steal it. No PID, process listing, stored
// socket address, command substring or signal is process ownership evidence.
// Only a read-only capability request is sent; a refusal leaves all hosts and
// the current daemon/selection unchanged. The launcher repeats this preflight
// after joining the old daemon, before executing the replacement.
func (s *State) CheckHosts(ctx context.Context) error {
	if err := RefuseCollision(s.state.Name(), "sessions"); err != nil {
		return err
	}
	sessions, err := nativehttp.OpenPrivateDir(s.state, "sessions", false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer sessions.Close()
	entries, err := os.ReadDir(sessions.Name())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != entry.Name() {
			return errors.New("desktop: ambiguous session directory")
		}
		dir, err := nativehttp.OpenPrivateDir(sessions, entry.Name(), false)
		if err != nil {
			return err
		}
		err = checkHost(ctx, dir)
		dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func checkHost(ctx context.Context, dir *os.Root) error {
	if err := RefuseCollision(dir.Name(), host.SpecFile); err != nil {
		return err
	}
	spec, specErr := nativehttp.OpenPrivateFile(dir, host.SpecFile, os.O_RDONLY)
	if specErr == nil {
		spec.Close()
	} else if !os.IsNotExist(specErr) {
		return specErr
	}
	if err := RefuseCollision(dir.Name(), host.LockFile); err != nil {
		return err
	}
	lease, err := nativehttp.OpenPrivateFile(dir, host.LockFile, os.O_RDWR)
	if os.IsNotExist(err) {
		if os.IsNotExist(specErr) {
			return nil
		}
		return errors.New("desktop: host ownership could not be established")
	}
	if err != nil {
		return err
	}
	defer lease.Close()
	err = syscall.Flock(int(lease.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		return nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return err
	}
	if specErr != nil {
		return errors.New("desktop: private host provenance is unavailable")
	}
	// Host's existing long-path pointer is private metadata, never a renderer
	// argument. Verify its name/object before the existing client reads it.
	if err := RefuseCollision(dir.Name(), "host.sock.path"); err != nil {
		return err
	}
	pointer, err := nativehttp.OpenPrivateFile(dir, "host.sock.path", os.O_RDONLY)
	if err == nil {
		pointer.Close()
	} else if !os.IsNotExist(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := host.DialContext(ctx, dir.Name())
	if err != nil {
		return errors.New("desktop: live host compatibility is unavailable")
	}
	defer c.Close()
	c.Deadline(time.Now().Add(2 * time.Second))
	capability, err := c.InputCapability()
	if err != nil || capability.Version != host.InputBatchVersion {
		return errors.New("desktop: live host protocol is incompatible; replacement refused")
	}
	return nil
}
