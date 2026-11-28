package workerinstall

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const aclGuardRemoved = "HATAORI_TEST_INSTALL_ACL_GUARD_REMOVED"

// These native tests run only in macOS CI. chmod creates ACLs only on each
// test's new disposable files/directories; no command output is parsed.
func TestDarwinACLChecks(t *testing.T) {
	if os.Getenv(aclGuardRemoved) == "1" {
		checkPlatformACL = func(string) error { return nil }
	}
	f := newReleaseFixture(t)
	for _, name := range []string{"ancestor", "prefix", "versions", "trust-key", "selected.json", "binary"} {
		t.Run(name, func(t *testing.T) {
			prefix := filepath.Join(privateTestDir(t), "install")
			old := mustInstall(t, prefix, f.request(t, "v1", nil))
			before, err := os.ReadFile(filepath.Join(prefix, "selected.json"))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(prefix, name)
			switch name {
			case "ancestor":
				target = filepath.Dir(prefix)
			case "prefix":
				target = prefix
			case "binary":
				target = old.Path
			}
			i, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			ace := "everyone allow write,append,delete,writeattr,writeextattr,writesecurity,chown"
			if i.IsDir() {
				ace = "everyone allow add_file,add_subdirectory,delete_child,search"
			}
			if out, err := exec.Command("/bin/chmod", "+a", ace, target).CombinedOutput(); err != nil {
				t.Fatalf("disposable ACL setup: %v %s", err, out)
			}
			defer func() {
				if out, err := exec.Command("/bin/chmod", "-N", target).CombinedOutput(); err != nil {
					t.Errorf("disposable ACL removal: %v %s", err, out)
				}
			}()
			if after, err := os.Stat(target); err != nil || after.Mode().Perm() != i.Mode().Perm() {
				t.Fatal("ACL setup changed POSIX mode; it must exercise the independent ACL guard")
			}
			if _, err := Install(prefix, f.request(t, "v2", nil)); err == nil {
				t.Fatal("unsafe ACL installation was accepted")
			}
			after, err := os.ReadFile(filepath.Join(prefix, "selected.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ACL refusal changed selection")
			}
		})
	}
}

func TestDarwinACLGuardRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDarwinACLChecks$", "-test.parallel=1")
	cmd.Env = append(os.Environ(), aclGuardRemoved+"=1", "GOMAXPROCS=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "unsafe ACL installation was accepted") {
		t.Fatalf("ACL guard removal did not produce its required assertion failure: %v %s", err, out)
	}
}

func TestDarwinNonMutatingACL(t *testing.T) {
	f := newReleaseFixture(t)
	prefix := filepath.Join(privateTestDir(t), "install")
	old := mustInstall(t, prefix, f.request(t, "v1", nil))
	if out, err := exec.Command("/bin/chmod", "+a", "everyone deny delete", prefix).CombinedOutput(); err != nil {
		t.Fatalf("disposable deny ACL: %v %s", err, out)
	}
	defer exec.Command("/bin/chmod", "-N", prefix).Run()
	selected, err := Selected(prefix, f.platform, "1.3")
	if err != nil || selected.Path != old.Path {
		t.Fatalf("protective deny ACL refused: %v", err)
	}
}
