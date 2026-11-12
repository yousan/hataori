package instance

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `{
	"owner": "owner-1",
	"trigger": "!go",
	"idle": "30m",
	"scopes": [{"key": "c1", "client": "chat", "enabled": true, "model": "m", "effort": "high", "posters": ["hook-1"]}],
	"drivers": {"chat": {"anything": [1, 2]}}
}`

// newRoot makes a root in a temporary directory with the given config.
func newRoot(t *testing.T, config string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOpenCreatesDirsUnderRoot(t *testing.T) {
	root := newRoot(t, validConfig)
	in, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer in.Close()
	for _, d := range []string{"state", "workspaces", "mirrors", LockFile} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if string(in.Config.Drivers["chat"]) != `{"anything": [1, 2]}` {
		t.Errorf("driver section = %q", in.Config.Drivers["chat"])
	}
}

// A config or a state directory that leads outside the root is refused.
func TestOpenRefusesPathsOutsideRoot(t *testing.T) {
	outside := t.TempDir()
	outsideConfig := filepath.Join(outside, "hataori.json")
	if err := os.WriteFile(outsideConfig, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("config", func(t *testing.T) {
		root := newRoot(t, validConfig)
		os.Remove(filepath.Join(root, ConfigFile))
		if err := os.Symlink(outsideConfig, filepath.Join(root, ConfigFile)); err != nil {
			t.Fatal(err)
		}
		if in, err := Open(root); err == nil {
			in.Close()
			t.Fatal("Open read a config outside the root")
		}
	})
	t.Run("state", func(t *testing.T) {
		root := newRoot(t, validConfig)
		if err := os.Symlink(outside, filepath.Join(root, "state")); err != nil {
			t.Fatal(err)
		}
		if in, err := Open(root); err == nil {
			in.Close()
			t.Fatal("Open wrote a lock outside the root")
		}
		if _, err := os.Stat(filepath.Join(outside, "lock")); err == nil {
			t.Fatal("a lock file was created outside the root")
		}
	})
}

func TestOpenRefusesFileInPlaceOfDir(t *testing.T) {
	root := newRoot(t, validConfig)
	if err := os.WriteFile(filepath.Join(root, "mirrors"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if in, err := Open(root); err == nil {
		in.Close()
		t.Fatal("Open accepted a file named mirrors")
	}
}

func TestConfigErrorsNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		name, config, key string
	}{
		{"missing", `{"trigger": "!go", "idle": "30m"}`, `"owner"`},
		{"missing in scope", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1"}]}`, `"scopes[0].client"`},
		{"unknown", `{"owner": "o", "trigger": "!go", "idle": "30m", "colour": "red"}`, `"colour"`},
		{"wrong type", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "enabled": "yes"}]}`, `enabled`},
		{"null", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "enabled": null}]}`, `"scopes[0].enabled"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.config))
			if err == nil {
				t.Fatal("ParseConfig succeeded")
			}
			msg := err.Error()
			if strings.Contains(msg, "\n") || !strings.Contains(msg, tc.key) {
				t.Fatalf("error %q is not one line naming %s", msg, tc.key)
			}
		})
	}
}

func TestConfigIgnoresSpacesAroundValues(t *testing.T) {
	c, err := ParseConfig([]byte(`{"owner": " o ", "trigger": "\t!go ", "idle": " 30m ",
		"scopes": [{"key": " c1 ", "client": " chat ", "model": " m ", "effort": " high", "posters": [" hook-1 "]}]}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	s := c.Scopes[0]
	if c.Owner != "o" || c.Trigger != "!go" || c.Idle != 30*time.Minute ||
		s.Key != "c1" || s.Client != "chat" || s.Model != "m" || s.Effort != "high" || s.Posters[0] != "hook-1" {
		t.Fatalf("spaces changed a value: %+v", c)
	}
}

func TestConfigReadableByOthersIsRefused(t *testing.T) {
	root := newRoot(t, validConfig)
	if err := os.Chmod(filepath.Join(root, ConfigFile), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := Open(root)
	if err == nil {
		in.Close()
		t.Fatal("Open accepted a config that others can read")
	}
	if !strings.Contains(err.Error(), "other users") {
		t.Fatalf("error %q does not say why", err)
	}
}

// TestLockHelper is not a test: TestSecondProcessIsRefused runs it in a
// child process to hold the lock.
func TestLockHelper(t *testing.T) {
	root := os.Getenv("HATAORI_TEST_LOCK_ROOT")
	if root == "" {
		t.Skip("helper process only")
	}
	in, err := Open(root)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	os.Stdin.Read(make([]byte, 1)) // hold the lock until the parent closes stdin
	in.Close()
	os.Exit(0)
}

func TestSecondProcessIsRefused(t *testing.T) {
	root := newRoot(t, validConfig)

	first := exec.Command(os.Args[0], "-test.run=^TestLockHelper$")
	first.Env = append(os.Environ(), "HATAORI_TEST_LOCK_ROOT="+root)
	stdin, _ := first.StdinPipe()
	stdout, _ := first.StdoutPipe()
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("first process: %q", line)
	}

	second := exec.Command(os.Args[0], "-test.run=^TestLockHelper$")
	second.Env = first.Env
	out, err := second.Output()
	if err == nil {
		t.Fatal("second process on the same root did not exit with an error")
	}
	if !strings.Contains(string(out), filepath.Join(root, LockFile)) {
		t.Fatalf("message %q does not name the lock", out)
	}

	stdin.Close()
	if err := first.Wait(); err != nil {
		t.Fatalf("first process: %v", err)
	}
	in, err := Open(root)
	if err != nil {
		t.Fatalf("Open after the first process exited: %v", err)
	}
	in.Close()
}
