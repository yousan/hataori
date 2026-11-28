package desktopstate

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestHostCompatibilityRefusesAmbiguousOrUnprovenOwnershipWithoutPIDs(t *testing.T) {
	for _, kind := range []string{"ambiguous", "link", "unknown-owner", "held-without-capability", "idle"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, rootFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			base := filepath.Join(s.state.Name(), "sessions")
			os.Mkdir(base, 0o700)
			name := "1"
			if kind == "ambiguous" {
				name = "01"
			}
			dir := filepath.Join(base, name)
			os.Mkdir(dir, 0o700)
			var file *os.File
			switch kind {
			case "link":
				os.Remove(dir)
				os.Symlink(rootFixture(t), dir)
			case "unknown-owner":
				os.WriteFile(filepath.Join(dir, "host.json"), []byte("inert existing spec"), 0o600)
			case "held-without-capability", "idle":
				file, err = os.OpenFile(filepath.Join(dir, "host.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if kind == "held-without-capability" {
					if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
					defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				}
			}
			err = s.CheckHosts(ctx)
			if kind == "idle" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unproven host accepted")
			}
		})
	}
}
