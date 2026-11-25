package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// The files of a scope's supervisor, which the daemon writes for its
// sessions (#868), through the control socket.

// rawCall sends one request of op with args, as they are, on a connection
// with token of its own, and returns the answer's result and error code.
func rawCall(t *testing.T, root, token, op string, args map[string]any) (json.RawMessage, string) {
	t.Helper()
	sock, err := wire.SocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if err := enc.Encode(map[string]any{"version": wire.Version, "token": token}); err != nil {
		t.Fatal(err)
	}
	var hello struct{ Version, Error string }
	if err := dec.Decode(&hello); err != nil || hello.Error != "" {
		t.Fatalf("hello: %v %s", err, hello.Error)
	}
	if err := enc.Encode(map[string]any{"id": 1, "op": op, "args": args}); err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Result json.RawMessage
		Error  *struct{ Code, Message string }
	}
	if err := dec.Decode(&ans); err != nil {
		t.Fatal(err)
	}
	if ans.Error != nil {
		return nil, ans.Error.Code
	}
	return ans.Result, ""
}

// Only a supervisor's connection has files: the owner's and a plugin's are
// answered not_supported. The scope and the thread are its token's, whatever
// the request names besides, and no path of one scope's leads into
// another's directory.
func TestFilesAreTheTokensScopes(t *testing.T) {
	plugin, _ := writePlugin(t, "ext", "exit 1")
	const pluginToken = "ext-token-Zr7q2"
	r, _ := svRoot(t, true, func(cfg map[string]any) {
		cfg["people"] = append(cfg["people"].([]map[string]any), map[string]any{"name": "sv2", "ids": []string{supervisor.ID("c2")}})
		c2 := cfg["scopes"].([]map[string]any)[1]
		c2["people"], c2["supervisor"] = []string{"dispatch", "sv2"}, map[string]any{"instructions": "Route the other channel's work."}
		cfg["plugins"] = []map[string]any{{"id": "ext", "path": plugin, "external": true, "token_file": "plugins/ext-token", "scopes": []string{"c1"}}}
	})
	if err := os.MkdirAll(filepath.Join(r.root, "config", "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "config", "plugins", "ext-token"), []byte(pluginToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := runDaemon(t, r.root)
	defer stop()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ext, err := wire.DialToken(r.root, pluginToken)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	ctx := context.Background()
	text := "x\n"
	sv, dir, token := map[string]control.ThreadInfo{}, map[string]string{}, map[string]string{}
	for _, scope := range []string{"c1", "c2"} {
		sv[scope] = supervisorThread(t, owner, scope, control.Idle)
		dir[scope], token[scope] = workspace.ScopePath(r.root, scope), tokenOf(t, r.root, sv[scope].ID)
	}

	for _, req := range []control.FilesRequest{{Op: control.FilesList}, {Op: control.FilesRead, Path: "AGENTS.md"},
		{Op: control.FilesWrite, Path: "AGENTS.md", Text: &text}, {Op: control.FilesEdit, Path: "AGENTS.md", Old: "a", New: &text}} {
		for who, c := range map[string]control.API{"the owner": owner, "a plugin": ext} {
			if _, err := c.Files(ctx, req); !errors.Is(err, control.ErrNotSupported) {
				t.Errorf("%s %ss: %v, want not supported", who, req.Op, err)
			}
		}
	}
	if _, code := rawCall(t, r.root, "", "Files", map[string]any{"op": control.FilesList, "scope": "c1", "thread": sv["c1"].ID, "token": token["c1"]}); code != "not_supported" {
		t.Errorf("the owner that names a scope: %q, want not_supported", code)
	}

	other := map[string]string{"c1": "c2", "c2": "c1"}
	for _, scope := range []string{"c1", "c2"} {
		o := other[scope]
		for path, text := range map[string]string{"AGENTS.md": "Of " + scope + ".\n", "notes/" + scope + ".md": "notes of " + scope + "\n"} {
			res, code := rawCall(t, r.root, token[scope], "Files", map[string]any{"op": control.FilesWrite, "path": path, "text": text,
				"scope": o, "thread": sv[o].ID, "dir": dir[o], "principal": "owner", "from": supervisor.ID(o)})
			var ans control.FilesAnswer
			if err := json.Unmarshal(res, &ans); code != "" || err != nil || ans.Refused != "" || ans.File == nil {
				t.Fatalf("%s writes %s: %q %v %+v", scope, path, code, err, ans)
			}
			if b, err := os.ReadFile(filepath.Join(dir[scope], path)); string(b) != text {
				t.Errorf("%s of %s holds %q, %v; want %q", path, scope, b, err, text)
			}
		}
		var threads string
		if err := dbOf(t, r.root).QueryRow(`SELECT group_concat(scope || ' ' || thread || ' ' || path, ', ' ORDER BY path) FROM supervisor_writes WHERE done = 1 AND scope = ?`,
			scope).Scan(&threads); err != nil || threads != fmt.Sprintf("%s %d AGENTS.md", scope, sv[scope].ID) {
			t.Errorf("the writes whose line is to come: %q, %v; want AGENTS.md's, of the token's thread", threads, err)
		}
	}
	for _, scope := range []string{"c1", "c2"} {
		c, err := wire.DialToken(r.root, token[scope])
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		ans, err := c.Files(ctx, control.FilesRequest{Op: control.FilesRead, Path: "AGENTS.md"})
		if err != nil || ans.File == nil || ans.File.Text != "Of "+scope+".\n" {
			t.Errorf("%s reads AGENTS.md: %+v, %v", scope, ans, err)
		}
		ans, err = c.Files(ctx, control.FilesRequest{Op: control.FilesList})
		var paths []string
		for _, f := range ans.Files {
			paths = append(paths, f.Path)
		}
		if all := " " + strings.Join(paths, " ") + " "; err != nil || !strings.Contains(all, " notes/"+scope+".md ") || strings.Contains(all, other[scope]+".md") {
			t.Errorf("%s lists %q, %v", scope, paths, err)
		}
		o := other[scope]
		rel, err := filepath.Rel(dir[scope], dir[o])
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{rel + "/notes/" + o + ".md", "notes/../" + rel + "/notes/" + o + ".md", dir[o] + "/notes/" + o + ".md"} {
			for _, req := range []control.FilesRequest{{Op: control.FilesRead, Path: p}, {Op: control.FilesWrite, Path: p, Text: &text},
				{Op: control.FilesEdit, Path: p, Old: "notes", New: &text}} {
				if ans, err := c.Files(ctx, req); err == nil {
					t.Errorf("%s %ss %s: %+v, want refused", scope, req.Op, p, ans)
				}
			}
		}
	}
	for _, scope := range []string{"c1", "c2"} {
		if b, err := os.ReadFile(filepath.Join(dir[scope], "notes", scope+".md")); string(b) != "notes of "+scope+"\n" {
			t.Errorf("the notes of %s at the end: %q, %v", scope, b, err)
		}
	}
}

// versionIn is the version that the answer of a file tool gives, and the
// text after it.
func versionIn(t *testing.T, answer string) (version, text string) {
	t.Helper()
	_, rest, ok := strings.Cut(answer, "), version ")
	version, _, dot := strings.Cut(rest, ".")
	_, text, line := strings.Cut(rest, "\n---\n")
	if !ok || !dot || !line {
		t.Fatalf("no version and text in %q", answer)
	}
	return version, text
}

// Two conversations read a file and both write it, at once: one write of the
// two changes nothing and its answer gives the other's content, and after it
// wrote again both changes are in the file. So are all when both go on.
func TestConversationsWriteOneFileAtOnce(t *testing.T) {
	f := newSvTalks(t, "")
	ids := map[string]control.ThreadID{"a": f.talk(t, "a"), "b": f.talk(t, "b")}
	m := map[string]*supervisor.MCP{"a": f.tools(t, ids["a"]), "b": f.tools(t, ids["b"])}
	const path = "notes/list.md"
	if out := mcpCall(t, m["a"], supervisor.ToolWrite, map[string]any{"path": path, "text": "The list:\n"}); out.IsError || !strings.HasPrefix(out.Text, "Written: ") {
		t.Fatalf("the file's first write: %+v", out)
	}
	const rounds = 5
	var refused [rounds]int
	for i := range rounds {
		version, text := versionIn(t, mcpCall(t, m["a"], supervisor.ToolFile, map[string]any{"path": path}).Text)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for who := range m {
			wg.Add(1)
			go func() {
				defer wg.Done()
				version, text := version, text
				for {
					out := mcpCall(t, m[who], supervisor.ToolWrite, map[string]any{"path": path, "version": version, "text": text + fmt.Sprintf("%s%d\n", who, i)})
					if strings.HasPrefix(out.Text, "Written: ") {
						return
					}
					if out.IsError || !strings.HasPrefix(out.Text, "Nothing was written: the file has changed since that version.") {
						t.Errorf("%s's write: %+v", who, out)
						return
					}
					mu.Lock()
					refused[i]++
					mu.Unlock()
					version, text = versionIn(t, out.Text)
				}
			}()
		}
		wg.Wait()
	}
	b, err := os.ReadFile(filepath.Join(workspace.ScopePath(f.wt.r.root, "c1"), path))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if err != nil || len(lines) != 1+2*rounds || lines[0] != "The list:" {
		t.Fatalf("the file holds %q, %v", b, err)
	}
	for i := range rounds {
		got := []string{lines[1+2*i], lines[2+2*i]}
		a, b := fmt.Sprint("a", i), fmt.Sprint("b", i)
		if x := strings.Join(got, " "); x != a+" "+b && x != b+" "+a {
			t.Errorf("round %d added %q; want %s and %s", i, x, a, b)
		}
	}
	if want := [rounds]int{1, 1, 1, 1, 1}; refused != want {
		t.Errorf("writes that did not fit, by round: %v; want one of the two each time", refused)
	}
}

// A daemon that starts removes what the one before it had made for a write
// that it did not come to the end of, before any session can write again:
// the file beside the old one, which is whole, and a directory made for a
// new one, with one that nobody may read yet. What is not the daemon's stays.
func TestDaemonsStartRemovesWhatAWriteLeft(t *testing.T) {
	r, _ := svRoot(t, true)
	stop := runDaemon(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	sv := supervisorThread(t, owner, "c1", control.Idle)
	writes(t, owner, sv.ID, "w", toolScript(t, supervisor.ToolWrite, map[string]any{"path": "notes/plan.md", "text": "the plan\n"}))
	owner.Close()
	stop()

	dir := threadDir(t, r.root, sv.ID)
	const own, person = ".hataori-0123456789abcdef", ".hataori-person"
	if err := os.MkdirAll(filepath.Join(dir, "new", "er"), 0o700); err != nil {
		t.Fatal(err)
	}
	const next = "the next plan\n"
	for name, text := range map[string]string{"notes/" + own: next, "new/er/" + own: "half", "notes/" + person: "a person's\n", "AGENTS.md": next} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "new/er", own), 0); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	version := func(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
	for path, w := range map[string]struct {
		was         string
		there, told int
	}{"notes/plan.md": {version("the plan\n"), 1, 1}, "new/er/file.md": {"", 0, 1}, "AGENTS.md": {"", 0, 1}} { // AGENTS.md has its place
		if _, err := db.Exec(`INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos, told) VALUES ('c1', ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			int64(sv.ID), dir, path, own, w.was, version(next), w.there, w.told); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := db.Exec(`UPDATE supervisor_wakes SET pos = (SELECT max(pos) FROM events) WHERE scope = 'c1'`); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("the waker's position is recorded %d times", n)
	}
	stop = runDaemon(t, r.root)
	defer stop()
	// The end of their turn was read before the daemon died, and what was
	// made is named nevertheless.
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	eventually(t, "the line of the write that was made", 10*time.Second, func() bool { return len(filesNotices(r, conv)) > 0 })
	if n := filesNotices(r, conv); len(n) != 1 || n[0] != supervisor.FilesNotice([]string{"AGENTS.md"}) {
		t.Errorf("the lines after the start: %q", n)
	}
	for name, there := range map[string]bool{"notes/" + own: false, "new": false, "notes/" + person: true, "notes/plan.md": true} {
		if _, err := os.Lstat(filepath.Join(dir, name)); (err == nil) != there {
			t.Errorf("%s after the start: %v, want it there: %v", name, err, there)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "notes", "plan.md")); string(b) != "the plan\n" {
		t.Errorf("the old file holds %q, %v", b, err)
	}
	var n int
	eventually(t, "nothing to be recorded", 10*time.Second, func() bool {
		return db.QueryRow(`SELECT count(*) FROM supervisor_writes`).Scan(&n) == nil && n == 0
	})
}
