//go:build darwin && cgo

package nativehttp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinACLGrantRefusesPrivatePaths(t *testing.T) {
	for _, target := range []string{"root", "state", "discovery", "socket", "ancestor", "runtime"} {
		t.Run(target, func(t *testing.T) {
			cfg := fixture(t)
			parent := cfg.Root
			if target == "ancestor" || target == "runtime" {
				cfg.Root = filepath.Join(parent, "instance")
				if target == "runtime" {
					cfg.Root = filepath.Join(parent, strings.Repeat("long-root-", 15))
					runtime, err := os.MkdirTemp("/tmp", "nr-")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.RemoveAll(runtime) })
					cfg.RuntimeDir = runtime
				}
				if err := os.Mkdir(cfg.Root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			l := listenFixture(t, cfg)
			path := cfg.Root
			permission := "search"
			switch target {
			case "state":
				path = filepath.Join(cfg.Root, "state")
			case "discovery":
				path = discoveryPath(cfg)
				permission = "read"
			case "socket":
				path = l.path
				permission = "write"
			case "ancestor":
				path = parent
			case "runtime":
				path = filepath.Dir(l.path)
			}
			if b, err := exec.Command("/bin/chmod", "+a", "everyone allow "+permission, path).CombinedOutput(); err != nil {
				t.Fatalf("fixture ACL: %v: %s", err, b)
			}
			t.Cleanup(func() { exec.Command("/bin/chmod", "-N", path).Run() })
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0o077 != 0 {
				t.Fatal("ACL fixture changed BSD private mode")
			}
			if err := checkACL(path); err == nil {
				t.Fatal("accepted ACL allow entry")
			}
			if conn, err := Dial(context.Background(), cfg); err == nil {
				conn.Close()
				t.Fatal("native Dial ignored ACL grant")
			}
		})
	}
}
