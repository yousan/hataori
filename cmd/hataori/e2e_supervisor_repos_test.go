package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// forgeAddress is a repository's address on a forge, which git is made to
// clone from a local repository instead (forged).
const forgeAddress = "https://forge.example/acme/widgets"

// forged makes git, in this process, clone forgeAddress from a local bare
// repository, and returns that repository.
func forged(t *testing.T) string {
	t.Helper()
	local := bareRepo(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+local+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", forgeAddress)
	return local
}

// mirrored reports whether the daemon of root made a mirror of repo, as
// it does for every clone.
func mirrored(t *testing.T, root, repo string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, "mirrors", workspace.MirrorKey(repo)+".git"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

var startedThread = regexp.MustCompile(`^mcp: Started thread #(\d+)\.$`)

// noReposLines are the daemon's lines saying that scope lists no repos.
func noReposLines(logged, scope string) int {
	n := 0
	for _, l := range strings.Split(logged, "\n") {
		if strings.Contains(l, `msg="the scope lists no supervisor.repos`) && strings.Contains(l, "scope="+scope) {
			n++
		}
	}
	return n
}

// The supervisor's start_thread starts a thread in a repository its scope
// lists, by the address listed and by its forge name, and refuses any
// other repository, which is not cloned: one listed by neither, a forge
// name not listed, and an address of another forge with a listed forge
// name. A start by the owner, and by a person of the scope through chat,
// is unchanged. list_threads and read_thread show each thread's
// repository, model and effort.
func TestSupervisorStartsOnlyInItsRepos(t *testing.T) {
	forged(t)
	listed, other := bareRepo(t), bareRepo(t)
	wt := newWakeTest(t, autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["repos"] = []string{listed, forgeAddress}
	})
	stop, logged := runDaemonLog(t, wt.r.root)
	defer stop()
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	wt.db = wt.r.db(t)
	sv := supervisorThread(t, owner, "c1", control.Idle)
	ctx := context.Background()
	threads := func() int {
		ts, err := owner.Threads(ctx, control.Filter{Scope: "c1"})
		if err != nil {
			t.Fatal(err)
		}
		return len(ts)
	}
	start := func(key, repo, model string) string {
		t.Helper()
		return sendAndRead(t, owner, sv.ID, key, fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": \"heard\\nreply started\", \"model\": %q, \"effort\": \"high\"}", repo, model))
	}
	started := func(answer, repo, model string) control.ThreadID {
		t.Helper()
		m := startedThread.FindStringSubmatch(answer)
		if m == nil {
			t.Fatalf("start_thread of %s answered %q, want a started thread", repo, answer)
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		id := control.ThreadID(n)
		wt.ended(t, id, 1)
		d, err := owner.Thread(ctx, id, 0, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		if d.Repo != repo || d.Model != model || d.Effort != "high" || d.StartedBy != "sv1" || d.Title != "heard" {
			t.Fatalf("the started thread: repository %q, model %q, effort %q, started by %q, title %q; want %q, %q, high, sv1, heard",
				d.Repo, d.Model, d.Effort, d.StartedBy, d.Title, repo, model)
		}
		// Its first input is the supervisor's, marked, and the agent heard
		// the mark first.
		inputs := threadEvents(t, owner, id, "input")
		if len(inputs) != 1 || inputs[0]["principal"] != "sv1" || !strings.HasPrefix(inputs[0]["text"].(string), supervisor.Mark+"\n") {
			t.Fatalf("the input events %v, want sv1's first input, marked", inputs)
		}
		if texts := threadEvents(t, owner, id, "text"); len(texts) == 0 || texts[0]["text"] != "heard: "+supervisor.Mark {
			t.Fatalf("the agent's text %v, want it to have heard the mark first", texts)
		}
		if !mirrored(t, wt.r.root, repo) {
			t.Fatalf("no mirror of %s after a start of it", repo)
		}
		return id
	}

	byAddress := started(start("s1", listed, "m-listed"), listed, "m-listed")
	byForge := started(start("s2", "ACME/Widgets", "m-forge"), forgeAddress, "m-forge")

	before := threads()
	for i, repo := range []string{other, "acme/other", "https://elsewhere.example/acme/widgets"} {
		a := start(fmt.Sprintf("r%d", i), repo, "m-refused")
		if !strings.HasPrefix(a, `mcp error: not allowed: the supervisor of scope "c1" may start a thread only in a repository its scope lists under supervisor.repos`) {
			t.Fatalf("start_thread of %q answered %q, want it refused, naming the scope", repo, a)
		}
	}
	time.Sleep(300 * time.Millisecond) // a start that went through would have its thread by now
	if n := threads(); n != before {
		t.Fatalf("%d threads in c1 after the refused starts, want %d", n, before)
	}
	for _, repo := range []string{other, "https://elsewhere.example/acme/widgets"} {
		if mirrored(t, wt.r.root, repo) {
			t.Fatalf("%s was cloned", repo)
		}
	}
	if !refusedLine(logged(), "Start", "c1") {
		t.Fatal("the refused starts were not logged")
	}
	if n := noReposLines(logged(), "c1"); n != 0 {
		t.Fatalf("%d lines say c1 lists no repos; it lists two", n)
	}

	// The owner, and a person of the scope through chat, start threads in
	// a repository the scope does not list, as before.
	mine, err := owner.Start(ctx, control.StartRequest{Key: "o", Scope: "c1", Thread: "to", Repo: other, Input: control.Input{Text: "reply by the owner"}})
	if err != nil {
		t.Fatalf("the owner's start of an unlisted repository: %v", err)
	}
	wt.ended(t, mine, 1)
	wt.r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+other+" reply by a person")
	var theirs control.ThreadInfo
	eventually(t, "the person's thread", 15*time.Second, func() bool {
		ts, err := owner.Threads(ctx, control.Filter{Scope: "c1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range ts {
			if th.Title == "reply by a person" && th.Repo == other && th.Turns == 1 && th.State == control.Idle {
				theirs = th
				return true
			}
		}
		return false
	})
	if theirs.StartedBy != "dispatch" {
		t.Fatalf("the thread from chat was started by %q, want dispatch", theirs.StartedBy)
	}

	// The supervisor sees which thread carries which repository, model and
	// effort.
	list := sendAndRead(t, owner, sv.ID, "l", "mcp list_threads")
	for _, w := range []struct {
		id          control.ThreadID
		repo, model string
	}{{byAddress, listed, "m-listed"}, {byForge, forgeAddress, "m-forge"}} {
		want := fmt.Sprintf("repository %s, model %s, effort high", w.repo, w.model)
		line := ""
		for _, l := range strings.Split(list, "\n") {
			if strings.HasPrefix(strings.TrimPrefix(l, "mcp: "), fmt.Sprintf("#%d ", w.id)) || strings.HasPrefix(l, fmt.Sprintf("#%d ", w.id)) {
				line = l
			}
		}
		if !strings.Contains(line, want) {
			t.Fatalf("list_threads shows thread %d as %q, want %q in it:\n%s", w.id, line, want, list)
		}
		read := sendAndRead(t, owner, sv.ID, fmt.Sprintf("r-%d", w.id), fmt.Sprintf("mcp read_thread {\"number\": %d}", w.id))
		if !strings.Contains(read, "\nRepository: "+w.repo+"\nModel: "+w.model+", effort: high\n") {
			t.Fatalf("read_thread of thread %d answered %q, want its repository, model and effort", w.id, read)
		}
	}
}

// A scope that lists no repos gets no start from its supervisor, and the
// daemon says so in one line at start; the owner's start is unchanged.
func TestSupervisorWithoutReposStartsNothing(t *testing.T) {
	wt := newWakeTest(t, func(cfg map[string]any) {
		delete(cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any), "repos")
	})
	stop, logged := runDaemonLog(t, wt.r.root)
	defer stop()
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	wt.db = wt.r.db(t)
	sv := supervisorThread(t, owner, "c1", control.Idle)
	a := sendAndRead(t, owner, sv.ID, "s", fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": \"reply started\"}", wt.repo))
	if !strings.HasPrefix(a, `mcp error: not allowed: the supervisor of scope "c1" starts no thread: its scope lists no repositories under supervisor.repos`) {
		t.Fatalf("start_thread with no repos answered %q", a)
	}
	time.Sleep(300 * time.Millisecond)
	if mirrored(t, wt.r.root, wt.repo) {
		t.Fatal("the repository was cloned")
	}
	if n := noReposLines(logged(), "c1"); n != 1 {
		t.Fatalf("%d lines say c1 lists no repos, want 1:\n%s", n, logged())
	}
	id, err := owner.Start(context.Background(), control.StartRequest{Key: "o", Scope: "c1", Thread: "to", Repo: wt.repo, Input: control.Input{Text: "reply by the owner"}})
	if err != nil {
		t.Fatalf("the owner's start: %v", err)
	}
	wt.ended(t, id, 1)
	if n := noReposLines(logged(), "c1"); n != 1 {
		t.Fatalf("%d lines say c1 lists no repos, want still 1", n)
	}
}
