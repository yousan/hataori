package instance

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		{"bad pattern", `{"owner": "o", "trigger": "!go", "idle": "30m", "cleanup": {"disposable": ["[x"]}}`, `"cleanup.disposable[0]"`},
		{"bad retention", `{"owner": "o", "trigger": "!go", "idle": "30m", "cleanup": {"trash_retention": "0s"}}`, `"cleanup.trash_retention"`},
		{"bad removal age", `{"owner": "o", "trigger": "!go", "idle": "30m", "cleanup": {"remove_closed_after": "a month"}}`, `"cleanup.remove_closed_after"`},
		{"relative root", `{"owner": "o", "trigger": "!go", "idle": "30m", "cleanup": {"other_roots": ["srv/other"]}}`, `"cleanup.other_roots[0]"`},
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

func TestCleanupConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The removal pass is off unless the config turns it on.
	if c.Cleanup.TrashRetention != DefaultTrashRetention || c.Cleanup.Disposable != nil || c.Cleanup.RemoveClosedAfter != 0 {
		t.Fatalf("defaults: %+v", c.Cleanup)
	}
	c, err = ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m",
		"cleanup": {"disposable": [" node_modules ", "*.pyc"], "trash_retention": "48h", "other_roots": ["/srv/other/"],
			"remove_closed_after": "720h"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%q %v %q %v", c.Cleanup.Disposable, c.Cleanup.TrashRetention, c.Cleanup.OtherRoots, c.Cleanup.RemoveClosedAfter); got != `["node_modules" "*.pyc"] 48h0m0s ["/srv/other"] 720h0m0s` {
		t.Fatal(got)
	}
}

func TestConfigIgnoresSpacesAroundValues(t *testing.T) {
	c, err := ParseConfig([]byte(`{"owner": " o ", "trigger": "\t!go ", "idle": " 30m ",
		"scopes": [{"key": " c1 ", "client": " chat ", "model": " m ", "effort": " high", "posters": [" hook-1 "]}], "drivers": {"chat": {}}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	s := c.Scopes[0]
	if name, _ := c.People.Resolve("chat:hook-1"); !slices.Equal(c.People.IDs(OldOwner, "chat"), []string{"o"}) || c.Trigger != "!go" || c.Idle != 30*time.Minute ||
		s.Key != "c1" || s.Client != "chat" || s.Model != "m" || s.Effort != "high" || !c.People.May(name, "c1") {
		t.Fatalf("spaces changed a value: %+v", c)
	}
}

const peopleConfig = `{"trigger": "!go", "idle": "30m", "owner": " ada ",
	"people": [
		{"name": "ada", "ids": ["chat:1", " other:U1 "]},
		{"name": "grace", "ids": ["chat:2"]},
		{"name": "hook", "ids": ["chat:9"]}
	],
	"scopes": [{"key": "c1", "client": "chat", "people": ["grace", " hook "]}, {"key": "c2", "client": "chat"}],
	"drivers": {"chat": {}, "other": {}}}`

// The owner may act everywhere, a scope's people in that scope only, and
// an id no person lists nowhere.
func TestPeopleConfig(t *testing.T) {
	c, err := ParseConfig([]byte(peopleConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if c.OldForm || c.Owner != "ada" {
		t.Fatalf("owner %q, old form %v", c.Owner, c.OldForm)
	}
	for _, tc := range []struct {
		id, scope string
		may       bool
	}{
		{"chat:1", "c1", true}, {"other:U1", "c2", true}, {"chat:2", "c1", true}, {"chat:2", "c2", false},
		{"chat:9", "c1", true}, {"other:2", "c1", false}, {"chat:3", "c1", false}, {"2", "c1", false},
	} {
		name, _ := c.People.Resolve(tc.id)
		if got := c.People.May(name, tc.scope); got != tc.may {
			t.Errorf("%s in %s: may = %v, want %v", tc.id, tc.scope, got, tc.may)
		}
	}
	if ids := c.People.IDs("ada", "other"); len(ids) != 1 || ids[0] != "U1" {
		t.Errorf("ada's ids on other: %v", ids)
	}
}

func TestPeopleConfigErrorsNameTheKey(t *testing.T) {
	base := `{"trigger": "!go", "idle": "30m", "owner": "ada", `
	for _, tc := range []struct {
		name, rest, key string
	}{
		{"no name", `"people": [{"ids": ["chat:1"]}]}`, `"people[0].name"`},
		{"colon in name", `"people": [{"name": "chat:1"}]}`, `"people[0].name"`},
		{"name twice", `"people": [{"name": "ada"}, {"name": "ada"}]}`, `"people[1].name"`},
		{"id without client", `"people": [{"name": "ada", "ids": ["1"]}]}`, `"people[0].ids[0]"`},
		{"id with empty part", `"people": [{"name": "ada", "ids": ["chat:"]}]}`, `"people[0].ids[0]"`},
		{"bad client", `"people": [{"name": "ada", "ids": ["Chat:1"]}]}`, `"people[0].ids[0]"`},
		{"id twice", `"people": [{"name": "ada", "ids": ["chat:1"]}, {"name": "b", "ids": ["chat:1"]}]}`, `"people[1].ids[0]"`},
		{"owner unknown", `"people": [{"name": "grace"}]}`, `"owner"`},
		{"scope person unknown", `"people": [{"name": "ada"}], "scopes": [{"key": "c1", "client": "chat", "people": ["x"]}]}`, `"scopes[0].people[0]"`},
		{"posters with people", `"people": [{"name": "ada"}], "scopes": [{"key": "c1", "client": "chat", "posters": ["1"]}]}`, `"scopes[0].posters"`},
		{"people without people", `"scopes": [{"key": "c1", "client": "chat", "people": ["ada"]}]}`, `"scopes[0].people"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(base + tc.rest))
			if err == nil {
				t.Fatal("ParseConfig succeeded")
			}
			if msg := err.Error(); strings.Contains(msg, "\n") || !strings.Contains(msg, tc.key) {
				t.Fatalf("error %q is not one line naming %s", msg, tc.key)
			}
		})
	}
}

// An old-form config allows exactly the authors it allowed before: the
// owner's id on every driver in every scope, and each poster on its
// scope's client in its scope; nothing else.
func TestOldFormAllowsTheSameAuthors(t *testing.T) {
	c, err := ParseConfig([]byte(`{"owner": "1", "trigger": "!go", "idle": "30m",
		"scopes": [
			{"key": "c1", "client": "chat", "posters": ["2", "3", "1"]},
			{"key": "c2", "client": "chat", "posters": ["3"]},
			{"key": "c3", "client": "other", "posters": ["2"]}
		],
		"drivers": {"chat": {}, "other": {}}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if !c.OldForm {
		t.Fatal("not read as the old form")
	}
	// What the old front ends allowed: the author's id is the owner's, or
	// one of the scope's posters, compared by the scope's client.
	old := func(client, local, scope string) bool {
		if local == "1" {
			return true
		}
		posters := map[string][]string{"c1": {"2", "3", "1"}, "c2": {"3"}, "c3": {"2"}}
		clients := map[string]string{"c1": "chat", "c2": "chat", "c3": "other"}
		return clients[scope] == client && slices.Contains(posters[scope], local)
	}
	for _, client := range []string{"chat", "other", "third"} {
		for _, local := range []string{"1", "2", "3", "4"} {
			for _, scope := range []string{"c1", "c2", "c3", "c4"} {
				if client == "third" && local == "1" {
					continue // no such driver: the old config compared nothing there
				}
				name, _ := c.People.Resolve(client + ":" + local)
				if got, want := c.People.May(name, scope), old(client, local, scope); got != want {
					t.Errorf("%s:%s in %s: may = %v, before %v", client, local, scope, got, want)
				}
			}
		}
	}
	if name, ok := c.People.Resolve("third:1"); ok {
		t.Errorf("third:1 is %q; the owner has ids only on configured drivers", name)
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

// The host's terminals stay off unless the config turns them on.
func TestHostTerminalsOffByDefault(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.HostTerminals {
		t.Error("host_terminals is on without the key")
	}
	on := strings.Replace(validConfig, "{", `{"host_terminals": true,`, 1)
	if c, err = ParseConfig([]byte(on)); err != nil || !c.HostTerminals {
		t.Errorf("host_terminals: true gives %v, %v", c, err)
	}
}
