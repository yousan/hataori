package instance

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		{"too few columns", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": 39, "rows": 24}}`, `"terminal_size.cols"`},
		{"too many columns", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": 501, "rows": 24}}`, `"terminal_size.cols"`},
		{"too few rows", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": 80, "rows": 9}}`, `"terminal_size.rows"`},
		{"too many rows", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": 80, "rows": 201}}`, `"terminal_size.rows"`},
		{"no rows", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": 80}}`, `"terminal_size.rows"`},
		{"no columns", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"rows": 30}}`, `"terminal_size.cols"`},
		{"not a number", `{"owner": "o", "trigger": "!go", "idle": "30m", "terminal_size": {"cols": "wide", "rows": 30}}`, `cols`},
		{"empty trust file", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "trust_files": [" "]}]}`, `"scopes[0].trust_files[0]"`},
		{"trust file with a space", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "trust_files": [".claude/my settings.json"]}]}`, `"scopes[0].trust_files[0]"`},
		{"trust file twice", `{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "trust_files": [".mcp.json", " .mcp.json"]}]}`, `"scopes[0].trust_files[1]"`},
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

// A process started by the one holding the instance has a copy of the
// lock's descriptor until it execs. Close releases the lock all the same,
// so the instance can be opened again at once.
func TestCloseReleasesTheLockAChildHasACopyOf(t *testing.T) {
	root := newRoot(t, validConfig)
	in, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// This child keeps its copy for as long as it runs, as a child does for
	// the moment between its fork and its exec.
	child := exec.Command("sleep", "60")
	child.ExtraFiles = []*os.File{in.lock}
	if err := child.Start(); err != nil {
		in.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	in.Close()
	again, err := Open(root)
	if err != nil {
		t.Fatalf("Open after Close, while a child has a copy of the lock's descriptor: %v", err)
	}
	again.Close()
}

// The host's terminals stay off unless the config turns them on.
// The terminal size is 0×0, for the host's 80×24, without the key, and
// the edges of the range are taken.
func TestTerminalSizeConfig(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	if err != nil || c.TerminalCols != 0 || c.TerminalRows != 0 {
		t.Fatalf("without terminal_size: %d×%d, %v", c.TerminalCols, c.TerminalRows, err)
	}
	for _, size := range [][2]int{{40, 10}, {500, 200}, {200, 50}} {
		with := strings.Replace(validConfig, "{", fmt.Sprintf(`{"terminal_size": {"cols": %d, "rows": %d},`, size[0], size[1]), 1)
		if c, err = ParseConfig([]byte(with)); err != nil || c.TerminalCols != size[0] || c.TerminalRows != size[1] {
			t.Fatalf("terminal_size %v: %d×%d, %v", size, c.TerminalCols, c.TerminalRows, err)
		}
	}
}

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

// host_input is off by default, needs host_terminals, and its idle period
// is 5 minutes by default and at least one second.
func TestHostInputConfig(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.HostInput || c.HostInputIdle != 5*time.Minute {
		t.Errorf("without the keys: host_input %v, host_input_idle %v", c.HostInput, c.HostInputIdle)
	}
	with := func(keys string) (*Config, error) {
		return ParseConfig([]byte(strings.Replace(validConfig, "{", "{"+keys+",", 1)))
	}
	_, err = with(`"host_input": true`)
	if err == nil || !strings.Contains(err.Error(), `"host_input"`) || !strings.Contains(err.Error(), `"host_terminals"`) {
		t.Errorf("host_input without host_terminals: %v", err)
	}
	if _, err = with(`"host_input": true, "host_terminals": false`); err == nil {
		t.Error("host_input with host_terminals false is accepted")
	}
	c, err = with(`"host_input": true, "host_terminals": true, "host_input_idle": " 90s "`)
	if err != nil || !c.HostInput || c.HostInputIdle != 90*time.Second {
		t.Errorf("host_input on, 90s: %+v, %v", c, err)
	}
	if c, err = with(`"host_input_idle": "1s"`); err != nil || c.HostInputIdle != time.Second {
		t.Errorf("host_input_idle 1s: %v", err)
	}
	for _, bad := range []string{`"999ms"`, `"1.5s"`, `"0s"`, `"-5m"`, `"five minutes"`, `"  "`} {
		_, err := with(`"host_input_idle": ` + bad)
		if err == nil || !strings.Contains(err.Error(), `"host_input_idle"`) {
			t.Errorf("host_input_idle %s: %v", bad, err)
		}
	}
}

// The dashboard section: "listen" is the loopback listener and "web" the
// web listener; either may be left out. A section in the old form, one
// listener with "url", "tls_cert", "tls_key" or "proxy" in it, is read as
// before: with TLS or a proxy the web listener alone, otherwise the
// loopback listener.
func TestDashboardSection(t *testing.T) {
	const base = `{"owner": "o", "trigger": "!go", "idle": "30m", "dashboard": `
	for _, tc := range []struct {
		name, section string
		want          Dashboard
	}{
		{"empty", `{}`, Dashboard{Local: &DashboardLocal{}}},
		{"loopback", `{"listen": "127.0.0.1:9000"}`, Dashboard{Local: &DashboardLocal{Listen: "127.0.0.1:9000"}}},
		{"web only", `{"web": {"listen": "127.0.0.1:9443", "url": "https://dash.example.com", "proxy": "127.0.0.1"}}`,
			Dashboard{Web: &DashboardWeb{Listen: "127.0.0.1:9443", URL: "https://dash.example.com", Proxy: "127.0.0.1", Key: "dashboard.web."}}},
		{"both", `{"listen": " 127.0.0.1:9000 ", "web": {"listen": "0.0.0.0:9443", "tls_cert": "c.pem", "tls_key": "k.pem", "url": "https://dash.example.com"}}`,
			Dashboard{Local: &DashboardLocal{Listen: "127.0.0.1:9000"},
				Web: &DashboardWeb{Listen: "0.0.0.0:9443", URL: "https://dash.example.com", TLSCert: "c.pem", TLSKey: "k.pem", Key: "dashboard.web."}}},
		{"old form, TLS", `{"listen": "0.0.0.0:7466", "url": "https://dash.example.com", "tls_cert": "c.pem", "tls_key": "k.pem"}`,
			Dashboard{OldForm: true, Web: &DashboardWeb{Listen: "0.0.0.0:7466", URL: "https://dash.example.com", TLSCert: "c.pem", TLSKey: "k.pem", Key: "dashboard."}}},
		{"old form, proxy", `{"url": "https://dash.example.com", "proxy": "127.0.0.1"}`,
			Dashboard{OldForm: true, Web: &DashboardWeb{URL: "https://dash.example.com", Proxy: "127.0.0.1", Key: "dashboard."}}},
		{"old form, loopback", `{"listen": "127.0.0.1:7466", "url": "http://localhost:7466"}`,
			Dashboard{OldForm: true, Local: &DashboardLocal{Listen: "127.0.0.1:7466", URL: "http://localhost:7466"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(base + tc.section + "}"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*c.Dashboard, tc.want) {
				t.Fatalf("%+v %+v %+v, want %+v %+v %+v", *c.Dashboard, c.Dashboard.Local, c.Dashboard.Web, tc.want, tc.want.Local, tc.want.Web)
			}
		})
	}
	for _, tc := range []struct{ name, section, key string }{
		{"web without an address", `{"web": {"proxy": "127.0.0.1", "url": "https://dash.example.com"}}`, `"dashboard.web.listen"`},
		{"half a certificate", `{"web": {"listen": "127.0.0.1:1", "tls_cert": "c.pem"}}`, `"dashboard.web.tls_cert" and "dashboard.web.tls_key"`},
		{"half a certificate, old form", `{"tls_key": "k.pem"}`, `"dashboard.tls_cert" and "dashboard.tls_key"`},
		{"both forms", `{"proxy": "127.0.0.1", "web": {"listen": "127.0.0.1:1"}}`, `"dashboard.web"`},
		{"unknown key in web", `{"web": {"listen": "127.0.0.1:1", "port": 1}}`, `"port"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(base + tc.section + "}"))
			if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("%v, want one line naming %s", err, tc.key)
			}
		})
	}
}

// The "plugins" list is handed over as it is written, nulls and unknown
// keys in its entries included: internal/plugin reads it. Without the key
// there is none.
func TestPluginsHandedOver(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	if err != nil || c.Plugins != nil {
		t.Fatalf("without plugins: %q, %v", c.Plugins, err)
	}
	entries := []string{`{"id": "a", "settings": {"level": null}}`, `{"anything": [1, 2]}`}
	with := strings.Replace(validConfig, "{", `{"plugins": [`+strings.Join(entries, ", ")+`],`, 1)
	if c, err = ParseConfig([]byte(with)); err != nil {
		t.Fatal(err)
	}
	if len(c.Plugins) != len(entries) || string(c.Plugins[0]) != entries[0] || string(c.Plugins[1]) != entries[1] {
		t.Fatalf("plugins = %q", c.Plugins)
	}
	for _, bad := range []string{`null`, `{}`, `"a"`} {
		_, err := ParseConfig([]byte(strings.Replace(validConfig, "{", `{"plugins": `+bad+`,`, 1)))
		if err == nil || !strings.Contains(err.Error(), `"plugins"`) {
			t.Errorf("plugins %s: %v", bad, err)
		}
	}
}

// A scope's trust_files are read as listed, trimmed; a scope without them
// trusts none (#423).
func TestTrustFilesConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [` +
		`{"key": "c1", "client": "chat", "trust_files": [".claude/settings.json", " .mcp.json "]}, {"key": "c2", "client": "chat"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Scopes[0].TrustFiles; !slices.Equal(got, []string{".claude/settings.json", ".mcp.json"}) {
		t.Fatalf("c1 trusts %q", got)
	}
	if got := c.Scopes[1].TrustFiles; got != nil {
		t.Fatalf("c2 trusts %q", got)
	}
}

// A supervisor's autonomy is propose without the key, as given with it,
// and any other value is refused at start, naming the key (#650).
func TestSupervisorAutonomyConfig(t *testing.T) {
	parse := func(sv string) (*Config, error) {
		return ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "supervisor": ` + sv + `}]}`))
	}
	c, err := parse(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Scopes[0].Supervisor.Autonomy; got != AutonomyPropose {
		t.Fatalf("without the key: %q, want %q", got, AutonomyPropose)
	}
	for _, mode := range []string{AutonomyWatch, AutonomyPropose, AutonomyAct} {
		c, err := parse(`{"autonomy": "` + mode + `"}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Scopes[0].Supervisor.Autonomy; got != mode {
			t.Fatalf("autonomy %q: %q", mode, got)
		}
	}
	if c, err := parse(`{"autonomy": " act "}`); err != nil || c.Scopes[0].Supervisor.Autonomy != AutonomyAct {
		t.Fatalf("autonomy with spaces: %v", err)
	}
	for _, bad := range []string{`"Act"`, `"auto"`, `"none"`} {
		_, err := parse(`{"autonomy": ` + bad + `}`)
		if err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.autonomy"`) {
			t.Fatalf("autonomy %s: %v", bad, err)
		}
	}
}

// A supervisor's files are absolute paths with base names of their own:
// two with the same base name, also when told apart by case only, are
// refused at start, naming both (#640). Without the key there are none.
func TestSupervisorFilesConfig(t *testing.T) {
	parse := func(sv string) (*Config, error) {
		return ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "supervisor": ` + sv + `}]}`))
	}
	c, err := parse(`{}`)
	if err != nil || c.Scopes[0].Supervisor.Files != nil {
		t.Fatalf("without the key: %v, %q", err, c.Scopes[0].Supervisor.Files)
	}
	c, err = parse(`{"files": ["/srv/a/design.md", "/srv/b/rules.md"]}`)
	if err != nil || !slices.Equal(c.Scopes[0].Supervisor.Files, []string{"/srv/a/design.md", "/srv/b/rules.md"}) {
		t.Fatalf("files: %v, %q", err, c.Scopes[0].Supervisor.Files)
	}
	for _, dup := range []string{`["/srv/a/design.md", "/srv/b/design.md"]`, `["/srv/a/design.md", "/srv/b/Design.md"]`} {
		_, err := parse(`{"files": ` + dup + `}`)
		if err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.files[1]"`) || !strings.Contains(err.Error(), "files[0]") ||
			!strings.Contains(err.Error(), "/srv/a/design.md") || !strings.Contains(err.Error(), "/srv/b/") {
			t.Fatalf("files %s: %v; want refused, naming both", dup, err)
		}
	}
	for _, bad := range []string{`""`, `"design.md"`, `" /srv/a.md"`} {
		_, err := parse(`{"files": [` + bad + `]}`)
		if err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.files[0]"`) {
			t.Fatalf("files [%s]: %v", bad, err)
		}
	}
}

// conversations is true without the key, and true or false (#871).
func TestSupervisorConversationsConfig(t *testing.T) {
	for sv, want := range map[string]string{`{}`: "true", `{"conversations": true}`: "true", `{"conversations": false}`: "false",
		`{"conversations": null}`: "refused", `{"conversations": "no"}`: "refused", `{"conversations": 0}`: "refused"} {
		c, err := ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "supervisor": ` + sv + `}]}`))
		if want == "refused" {
			if err == nil || !strings.Contains(err.Error(), "conversations") {
				t.Errorf("%s: %v; want refused, naming the key", sv, err)
			}
			continue
		}
		if err != nil || fmt.Sprint(c.Scopes[0].Supervisor.Conversations) != want {
			t.Errorf("%s: %v, want %s", sv, err, want)
		}
	}
}

// conversations_at_once is 3 without the key, and 1 or more (#875).
func TestSupervisorConversationsAtOnceConfig(t *testing.T) {
	for sv, want := range map[string]int{`{}`: 3, `{"conversations_at_once": 1}`: 1, `{"conversations_at_once": 12}`: 12,
		`{"conversations": false}`: 3, `{"conversations_at_once": 0}`: -1, `{"conversations_at_once": -2}`: -1} {
		c, err := ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "supervisor": ` + sv + `}]}`))
		if want < 0 {
			if err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.conversations_at_once"`) {
				t.Errorf("%s: %v; want refused", sv, err)
			}
			continue
		}
		if err != nil || c.Scopes[0].Supervisor.ConversationsAtOnce != want {
			t.Errorf("%s: %v, want %d", sv, err, want)
		}
	}
}

// reminders_max is 10 without the key, and 0 or more (#649).
func TestSupervisorRemindersMaxConfig(t *testing.T) {
	for sv, want := range map[string]int{`{}`: 10, `{"reminders_max": 3}`: 3, `{"reminders_max": 0}`: 0, `{"reminders_max": -1}`: -1} {
		c, err := ParseConfig([]byte(`{"owner": "o", "trigger": "!go", "idle": "30m", "scopes": [{"key": "c1", "client": "chat", "supervisor": ` + sv + `}]}`))
		if want < 0 {
			if err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.reminders_max"`) {
				t.Fatalf("%s: %v; want refused", sv, err)
			}
			continue
		}
		if err != nil || c.Scopes[0].Supervisor.RemindersMax != want {
			t.Fatalf("%s: %v, want %d", sv, err, want)
		}
	}
}
