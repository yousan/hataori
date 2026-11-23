package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/forge"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// fakeForgeName is the forge the tests read: in memory, serving the
// addresses on forge.example.
const fakeForgeName = "fake-forge"

// fakeForgeToken is the token of the fake forge's token file: a marker no
// other place may hold.
const fakeForgeToken = "MARKERTOKEN_9f3c1e7a5b2d4f6081a3c5e7"

// The fake forge's data, and the reads it was asked for.
var fakeForge struct {
	sync.Mutex
	token  string // what its factory read from its token file
	reads  []string
	issues []forge.IssueDetail
	pulls  []forge.PullDetail
	checks forge.Checks
}

// fakeForgeClient keeps its token in a field, as a forge does, so that a
// value of it printed anywhere shows the token.
type fakeForgeClient struct{ token string }

// newFakeForge is the fake forge's factory: it reads its token file from
// the config directory, as a forge does, and keeps the token in memory.
func newFakeForge(config *os.Root, settings []byte) (forge.Forge, error) {
	var s struct {
		TokenFile string `json:"token_file"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		return nil, err
	}
	f, err := config.Open(s.TokenFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(b))
	fakeForge.Lock()
	fakeForge.token = token
	fakeForge.Unlock()
	return fakeForgeClient{token: token}, nil
}

func (fakeForgeClient) read(what string) {
	fakeForge.Lock()
	defer fakeForge.Unlock()
	fakeForge.reads = append(fakeForge.reads, what)
}

func (fakeForgeClient) Serves(address string) (forge.Repo, bool) {
	rest, ok := strings.CutPrefix(address, "https://forge.example/")
	if !ok {
		return forge.Repo{}, false
	}
	r, err := forge.ParseRepo(rest)
	return r, err == nil
}

func (c fakeForgeClient) Issues(ctx context.Context, repo forge.Repo, state forge.State) (forge.List[forge.Issue], error) {
	c.read("issues " + repo.String())
	fakeForge.Lock()
	defer fakeForge.Unlock()
	var out forge.List[forge.Issue]
	for _, i := range fakeForge.issues {
		out.Items = append(out.Items, i.Issue)
	}
	return out, nil
}

func (c fakeForgeClient) Issue(ctx context.Context, repo forge.Repo, n int) (forge.IssueDetail, error) {
	c.read(fmt.Sprintf("issue %s %d", repo, n))
	fakeForge.Lock()
	defer fakeForge.Unlock()
	for _, i := range fakeForge.issues {
		if i.Number == n {
			return i, nil
		}
	}
	return forge.IssueDetail{}, &forge.Error{Status: 404, Message: "Not Found"}
}

func (c fakeForgeClient) Pulls(ctx context.Context, repo forge.Repo, state forge.State) (forge.List[forge.Pull], error) {
	c.read("pulls " + repo.String())
	fakeForge.Lock()
	defer fakeForge.Unlock()
	var out forge.List[forge.Pull]
	for _, p := range fakeForge.pulls {
		out.Items = append(out.Items, p.Pull)
	}
	return out, nil
}

func (c fakeForgeClient) Pull(ctx context.Context, repo forge.Repo, n int) (forge.PullDetail, error) {
	c.read(fmt.Sprintf("pull %s %d", repo, n))
	fakeForge.Lock()
	defer fakeForge.Unlock()
	for _, p := range fakeForge.pulls {
		if p.Number == n {
			return p, nil
		}
	}
	return forge.PullDetail{}, &forge.Error{Status: 403, RateLimited: true, Message: "API rate limit exceeded"}
}

func (c fakeForgeClient) Checks(ctx context.Context, repo forge.Repo, ref string) (forge.Checks, error) {
	c.read(fmt.Sprintf("checks %s %s", repo, ref))
	fakeForge.Lock()
	defer fakeForge.Unlock()
	return fakeForge.checks, nil
}

// forgeReads returns how many reads the fake forge was asked for.
func forgeReads() int {
	fakeForge.Lock()
	defer fakeForge.Unlock()
	return len(fakeForge.reads)
}

// strangerMarker starts every text the fake forge's stranger wrote.
const strangerMarker = "MARKERSTRANGER"

// fillFakeForge gives the fake forge its repository's data: items by
// alice, who has write access, and by stranger, who has none, whose texts
// hold strangerMarker; and an issue whose body is longer than a forge
// tool's answer.
func fillFakeForge() {
	alice := forge.Author{Login: "alice", Write: true}
	stranger := forge.Author{Login: "stranger"}
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	fakeForge.Lock()
	defer fakeForge.Unlock()
	fakeForge.reads = nil
	fakeForge.issues = []forge.IssueDetail{
		{Issue: forge.Issue{Number: 1, Title: strangerMarker + "-issue-title", State: "open", Author: stranger, Updated: at, URL: "https://forge.example/acme/widgets/issues/1"},
			Body: strangerMarker + "-issue-body", Comments: forge.List[forge.Comment]{Items: []forge.Comment{
				{Author: alice, Body: "Alice looked at it.", Created: at, URL: "https://forge.example/acme/widgets/issues/1#c1"},
				{Author: stranger, Body: strangerMarker + "-comment", Created: at, URL: "https://forge.example/acme/widgets/issues/1#c2"}}}},
		{Issue: forge.Issue{Number: 2, Title: "A visible title", State: "open", Author: alice, Labels: []string{"bug"}, Updated: at,
			URL: "https://forge.example/acme/widgets/issues/2"}, Body: "Alice's body."},
		{Issue: forge.Issue{Number: 5, Title: "A long one", State: "open", Author: alice, Updated: at, URL: "https://forge.example/acme/widgets/issues/5"},
			Body: strings.Repeat("All of this is Alice's.\n", 1200)},
	}
	fakeForge.pulls = []forge.PullDetail{
		{Pull: forge.Pull{Number: 3, Title: "Fix the build", State: "open", Branch: "fix-build", Base: "main", Author: alice, Updated: at,
			URL: "https://forge.example/acme/widgets/pull/3"}, Body: "Closes #2.", Head: "abc123", MergeState: "clean",
			Reviews: forge.List[forge.Review]{Items: []forge.Review{
				{Author: alice, State: "APPROVED", Body: "Looks good to Alice.", Submitted: at},
				{Author: stranger, State: "COMMENTED", Body: strangerMarker + "-review", Submitted: at}}},
			Comments: forge.List[forge.Comment]{Items: []forge.Comment{{Author: stranger, Body: strangerMarker + "-pull-comment", Created: at}}},
			ReviewComments: forge.List[forge.Comment]{Items: []forge.Comment{
				{Author: alice, Body: "Alice asks why.", Path: "build.sh", Created: at},
				{Author: stranger, Body: strangerMarker + "-line-comment", Path: "build.sh", Created: at}}},
			Files:  forge.List[forge.File]{Items: []forge.File{{Path: "build.sh", Status: "modified", Additions: 3, Deletions: 1}}},
			Checks: forge.Checks{Commit: "abc123", State: "success", Items: forge.List[forge.Check]{Items: []forge.Check{{Kind: forge.CheckRun, Name: "unit tests", State: "completed", Conclusion: "success"}}}}},
		{Pull: forge.Pull{Number: 4, Title: strangerMarker + "-pull-title", State: "open", Branch: strangerMarker + "-branch", Base: "main", Author: stranger,
			Updated: at, URL: "https://forge.example/acme/widgets/pull/4"}, Body: strangerMarker + "-pull-body", Head: "def456",
			Files:          forge.List[forge.File]{Items: []forge.File{{Path: strangerMarker + "-path", Previous: strangerMarker + "-was", Status: "renamed", Additions: 1}}},
			ReviewComments: forge.List[forge.Comment]{Items: []forge.Comment{{Author: alice, Body: "Alice looked.", Path: strangerMarker + "-path", Created: at}}},
			Checks: forge.Checks{Commit: "def456", Items: forge.List[forge.Check]{Items: []forge.Check{
				{Kind: forge.CheckRun, Name: strangerMarker + "-check", State: "completed", Conclusion: "success"}}}}},
	}
	fakeForge.checks = forge.Checks{Commit: "abc123", State: "pending", Items: forge.List[forge.Check]{Items: []forge.Check{
		{Kind: forge.CheckRun, Name: "lint", State: "completed", Conclusion: "failure", URL: "https://forge.example/acme/widgets/runs/9"}}}}
}

// forgeConfig lists, for c1's supervisor, a repository on the fake forge,
// one on a forge the instance does not read, and one with no forge name,
// and names the fake forge with its token file.
func forgeConfig(local string, trusted ...string) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["repos"] = []string{forgeAddress, "https://elsewhere.example/acme/gadgets", local}
		if len(trusted) > 0 {
			sv["trusted_authors"] = trusted
		}
		cfg["forges"] = map[string]any{fakeForgeName: map[string]any{"token_file": "forge-token"}}
	}
}

// changeConfig rewrites root's config with change.
func changeConfig(t *testing.T, root string, change func(cfg map[string]any)) {
	t.Helper()
	path := filepath.Join(root, instance.ConfigFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	var scopes []map[string]any
	for _, s := range cfg["scopes"].([]any) {
		scopes = append(scopes, s.(map[string]any))
	}
	cfg["scopes"] = scopes
	change(cfg)
	if b, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ask sends text to thread id as the owner and returns the final answer
// of the turn it starts, counting the turns in the store, which keeps
// them all.
func (wt *wakeTest) ask(t *testing.T, owner *wire.Client, id control.ThreadID, key, text string) string {
	t.Helper()
	var ended int
	if err := wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'turn_ended'`, int64(id)).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if err := owner.Send(context.Background(), id, key, control.Input{Text: text}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	pos := wt.ended(t, id, ended+1)
	var data string
	if err := wt.db.QueryRow(`SELECT data FROM events WHERE thread_id = ? AND kind = 'text' AND pos < ? ORDER BY pos DESC LIMIT 1`,
		int64(id), pos).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var m struct{ Text string }
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatal(err)
	}
	return m.Text
}

// eventsHolding counts the events of root's store whose data holds s.
func eventsHolding(t *testing.T, wt *wakeTest, s string) int {
	t.Helper()
	var n int
	if err := wt.db.QueryRow(`SELECT count(*) FROM events WHERE instr(data, ?) > 0`, s).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The supervisor's forge tools read a listed repository on the forge that
// serves it, and refuse, reading nothing, a repository not listed, one on
// no forge of the instance, one with no forge name, and a caller that is
// not a supervisor's. The text of an author without write access is in no
// answer and no event until the scope trusts that login. The forge's token
// is under no path of the root but its own file, and in no event and no log
// line. An answer over the size limit is cut, with the line that says so.
func TestSupervisorForgeTools(t *testing.T) {
	fillFakeForge()
	local := bareRepo(t)
	wt := newWakeTest(t, forgeConfig(local))
	if err := os.WriteFile(filepath.Join(wt.r.root, "config", "forge-token"), []byte(fakeForgeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop, logged := runDaemonLog(t, wt.r.root)
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	wt.db = wt.r.db(t)
	sv := supervisorThread(t, owner, "c1", control.Idle)
	fakeForge.Lock()
	if fakeForge.token != fakeForgeToken {
		t.Errorf("the forge read the token %q from its file", fakeForge.token)
	}
	fakeForge.Unlock()
	n := 0
	call := func(tool string, args map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(args)
		n++
		return wt.ask(t, owner, sv.ID, fmt.Sprintf("f%d", n), "mcp "+tool+" "+string(b))
	}

	// Each tool reads the listed repository, by its forge name and by its
	// address, and shows what alice wrote.
	reads := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{supervisor.ToolIssues, map[string]any{"repo": "acme/widgets"}, []string{"Issues of acme/widgets (open): 3", `#2 open "A visible title", by alice (write access)`, "labels: bug"}},
		{supervisor.ToolIssue, map[string]any{"repo": forgeAddress, "number": 1}, []string{"Issue #1 of acme/widgets", "Alice looked at it."}},
		{supervisor.ToolPulls, map[string]any{"repo": "ACME/widgets", "state": "all"}, []string{"Pull requests of acme/widgets (all): 2", `#3 open "Fix the build", branch fix-build, into main`}},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 3}, []string{"Pull request #3 of acme/widgets", "Looks good to Alice.", "modified build.sh +3 -1", "check_run unit tests: completed, success"}},
		{supervisor.ToolChecks, map[string]any{"repo": "acme/widgets", "ref": "main"}, []string{"Checks of main in acme/widgets", "combined status: pending", "check_run lint: completed, failure; https://forge.example/acme/widgets/runs/9"}},
	}
	for _, r := range reads {
		a := call(r.tool, r.args)
		for _, w := range r.want {
			if !strings.Contains(a, w) {
				t.Fatalf("%s %v answered %q, want %q in it", r.tool, r.args, a, w)
			}
		}
	}
	if got := forgeReads(); got != len(reads) {
		t.Fatalf("%d reads of the forge, want %d", got, len(reads))
	}

	// The stranger's texts are withheld, each naming its author.
	for _, r := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{supervisor.ToolIssues, map[string]any{"repo": "acme/widgets"}, "#1 open (Its title is withheld: stranger has no write access to the repository"},
		{supervisor.ToolIssue, map[string]any{"repo": "acme/widgets", "number": 1}, "(Its text is withheld: stranger has no write access"},
		{supervisor.ToolPulls, map[string]any{"repo": "acme/widgets"}, "#4 open (Its title and branch are withheld: stranger"},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 3}, "COMMENTED by stranger (no write access)"},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 3}, "- by alice (write access) on build.sh"},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 4}, "(Its body is withheld: stranger"},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 4}, "- renamed (path withheld) +1 -0"},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 4}, "- check_run (name withheld): completed, success"},
	} {
		if a := call(r.tool, r.args); strings.Contains(a, strangerMarker) || !strings.Contains(a, r.want) {
			t.Fatalf("%s %v answered %q; want %q in it and no text of stranger's", r.tool, r.args, a, r.want)
		}
	}

	// The checks of a commit, or of a pull request's ref, may be named by
	// whoever wrote the commit: their names are withheld.
	fakeForge.Lock()
	const named = "deploy-preview-of-the-commit"
	fakeForge.checks.Items.Items = append(fakeForge.checks.Items.Items, forge.Check{Kind: forge.CommitStatus, Name: named, State: "success",
		URL: "https://ci.example/" + named})
	fakeForge.Unlock()
	for _, ref := range []string{"def456", "0123456789abcdef0123456789abcdef01234567", "pull/4/head", "refs/pull/4/merge"} {
		a := call(supervisor.ToolChecks, map[string]any{"repo": "acme/widgets", "ref": ref})
		if strings.Contains(a, named) || strings.Contains(a, "lint") || strings.Contains(a, "runs/9") || !strings.Contains(a, "- status (name withheld): success\n") ||
			!strings.Contains(a, "(The names of these checks are withheld") {
			t.Fatalf("read_checks of %s answered %q, want the names withheld", ref, a)
		}
	}
	if a := call(supervisor.ToolChecks, map[string]any{"repo": "acme/widgets", "ref": "release/1.2"}); !strings.Contains(a, "- status "+named+": success") {
		t.Fatalf("read_checks of a branch answered %q, want the names", a)
	}
	fakeForge.Lock()
	fakeForge.checks.Items.Items = fakeForge.checks.Items.Items[:1]
	fakeForge.Unlock()

	// Refused, reading nothing: a repository not listed, one on no forge
	// of the instance, one with no forge name.
	before := forgeReads()
	for tool, extra := range map[string]map[string]any{supervisor.ToolIssues: {}, supervisor.ToolIssue: {"number": 1},
		supervisor.ToolPulls: {}, supervisor.ToolPull: {"number": 3}, supervisor.ToolChecks: {"ref": "main"}} {
		for repo, want := range map[string]string{
			"acme/other":          `mcp error: not allowed: the supervisor of scope "c1" reads a forge only for a repository its scope lists`,
			"acme/gadgets":        `mcp error: not allowed: the supervisor of scope "c1" cannot read this repository on a forge`,
			local:                 `mcp error: not allowed: the supervisor of scope "c1" cannot read this repository on a forge`,
			forgeAddress + ".git": `mcp error: not allowed: the supervisor of scope "c1" reads a forge only`,
		} {
			args := map[string]any{"repo": repo}
			for k, v := range extra {
				args[k] = v
			}
			if a := call(tool, args); !strings.HasPrefix(a, want) {
				t.Fatalf("%s of %q answered %q, want %q", tool, repo, a, want)
			}
		}
	}
	// Any other caller: the owner's connection reaches the core.
	for _, read := range []string{control.ForgeReadIssues, control.ForgeReadIssue, control.ForgeReadPulls, control.ForgeReadPull, control.ForgeReadChecks} {
		if _, err := owner.Forge(context.Background(), control.ForgeRequest{Repo: "acme/widgets", Read: read, Number: 1, Ref: "main"}); !errors.Is(err, control.ErrNotSupported) {
			t.Fatalf("the owner's Forge %s: %v, want not supported", read, err)
		}
	}
	if got := forgeReads(); got != before {
		t.Fatalf("%d reads of the forge for refused calls", got-before)
	}

	// A forge's refusal is a tool error with its reason.
	if a := call(supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 99}); !strings.HasPrefix(a, "mcp error: failed: the forge refused the read: forge: rate limited (403): API rate limit exceeded") {
		t.Fatalf("a rate-limited read answered %q", a)
	}

	// An answer over the limit is cut, with the line that says so.
	long := call(supervisor.ToolIssue, map[string]any{"repo": "acme/widgets", "number": 5})
	if !strings.HasSuffix(long, fmt.Sprintf("(Cut: the answer is longer than %d bytes; only its beginning is shown.)", supervisor.ForgeMax)) ||
		len(long) > len("mcp: ")+supervisor.ForgeMax {
		t.Fatalf("a long answer, %d bytes, ends %q", len(long), long[max(0, len(long)-200):])
	}

	// No stranger's text in any event, and the token nowhere.
	if n := eventsHolding(t, wt, strangerMarker); n != 0 {
		t.Fatalf("%d events hold the stranger's text", n)
	}
	noToken(t, wt, logged())
	owner.Close()
	stop()

	// Trusted, the stranger's texts are given.
	changeConfig(t, wt.r.root, func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["trusted_authors"] = []string{"Stranger"}
	})
	stop, logged = runDaemonLog(t, wt.r.root)
	defer stop()
	owner, err = wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	sv = supervisorThread(t, owner, "c1", control.Idle)
	for _, r := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{supervisor.ToolIssues, map[string]any{"repo": "acme/widgets"}, []string{strangerMarker + "-issue-title"}},
		{supervisor.ToolIssue, map[string]any{"repo": "acme/widgets", "number": 1}, []string{strangerMarker + "-issue-body", strangerMarker + "-comment"}},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 3}, []string{strangerMarker + "-review", strangerMarker + "-pull-comment", strangerMarker + "-line-comment"}},
		{supervisor.ToolPull, map[string]any{"repo": "acme/widgets", "number": 4}, []string{strangerMarker + "-pull-title", strangerMarker + "-branch", strangerMarker + "-pull-body",
			strangerMarker + "-path +1 -0 (was " + strangerMarker + "-was)", "on " + strangerMarker + "-path", strangerMarker + "-check"}},
	} {
		a := call(r.tool, r.args)
		for _, w := range r.want {
			if !strings.Contains(a, w) {
				t.Fatalf("trusted, %s %v answered %q, want %q in it", r.tool, r.args, a, w)
			}
		}
	}
	noToken(t, wt, logged())
}

// noToken fails when the fake forge's token is under any path of the root
// but its own file, in an event or in what the daemon logged; and, where
// the system shows them, in the environment of a process of the root.
func noToken(t *testing.T, wt *wakeTest, logged string) {
	t.Helper()
	if strings.Contains(logged, fakeForgeToken) {
		t.Fatal("the daemon logged the forge's token")
	}
	if n := eventsHolding(t, wt, fakeForgeToken); n != 0 {
		t.Fatalf("%d events hold the forge's token", n)
	}
	own := filepath.Join(wt.r.root, "config", "forge-token")
	err := filepath.WalkDir(wt.r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == own || d.Type()&fs.ModeType != 0 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(fakeForgeToken)) {
			t.Errorf("%s holds the forge's token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The session's MCP config is one of those files; it names the token
	// of the supervisor's connection, so it was looked at.
	found := false
	filepath.WalkDir(wt.r.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(supervisor.TokenEnv)) {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Fatal("no file of the root names the supervisor's MCP server's token: the walk missed the MCP config")
	}
	if runtime.GOOS != "linux" {
		return
	}
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		cmd, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmd, []byte(wt.r.root)) {
			continue
		}
		if env, err := os.ReadFile(filepath.Join("/proc", p.Name(), "environ")); err == nil && bytes.Contains(env, []byte(fakeForgeToken)) {
			t.Errorf("process %s has the forge's token in its environment", p.Name())
		}
	}
}
