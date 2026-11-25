package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/workspace"
)

// TestRealClaudeCodeManager checks hataori's layer of a supervisor's
// instructions (#653) with the real Claude Code CLI, on a scope model at
// effort high, and runs only when asked for (docs/testing.md):
//
//	HATAORI_REAL_CLAUDE=1 go test -run TestRealClaudeCodeManager -v -timeout 40m ./cmd/hataori
//
// The channel's USER.md asks for Japanese. Each step is judged by what
// happened, and logs what the supervisor answered. A step that fails does
// not stop the others. Words that only a file holds tell whether it was
// read.
func TestRealClaudeCodeManager(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the check against the real CLI")
	}
	settingsBefore := userSettings(t)
	if v, err := exec.Command("claude", "--version").Output(); err == nil {
		t.Logf("CLI %s", strings.TrimSpace(string(v)))
	}
	bin := t.TempDir()
	phase1 := filepath.Join(bin, "claude-for-the-check")
	wrapper := filepath.Join(bin, "claude-for-the-supervisor-check")
	if err := os.WriteFile(phase1, []byte(realWrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf(realSupervisorWrapper, phase1)), 0o700); err != nil {
		t.Fatal(err)
	}
	word := func(n int64) string { return "w" + strconv.FormatInt(time.Now().UnixNano()+n, 36) }
	docWord, noteWord, repoWord := word(1), word(2), word(3)
	// A repository with a file that none of the supervisor's tools shows.
	repo := bareRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	for _, args := range [][]string{{"clone", "-q", repo, clone}, {"-C", clone, "config", "user.email", "check@example.com"}, {"-C", clone, "config", "user.name", "check"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(clone, "PASSPHRASE.txt"), []byte(repoWord+"\n"), 0o600)
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "a passphrase"}, {"push", "-q", "origin", "HEAD"}} {
		if out, err := exec.Command("git", append([]string{"-C", clone}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	doc := filepath.Join(t.TempDir(), "rules.md")
	os.WriteFile(doc, []byte("# The project's rules\n\nThe release codename is "+docWord+".\n"), 0o600)

	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["login_relay"] = false
		cfg["owner"] = "boss"
		cfg["model"] = "sonnet"
		cfg["people"] = []map[string]any{
			{"name": "boss", "ids": []string{remoteSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{remoteSurface + ":" + e2eHook}},
			{"name": "sv1", "ids": []string{supervisor.ID("c1")}},
		}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": remoteSurface, "enabled": true, "people": []string{"dispatch", "sv1"},
			"supervisor": map[string]any{"quiet": "2s", "model": "sonnet", "effort": "high", "autonomy": "act", "repos": []string{repo},
				"files": []string{doc}, "turns_per_hour": 0, "sends_per_hour": 0}}}
		cfg["adapters"] = map[string]any{claudecode.Name: map[string]any{"command": wrapper}}
	})
	dir := workspace.ScopePath(p.root, "c1")
	for name, text := range map[string]string{
		"USER.md":       "The people here read Japanese. Write to them in Japanese.\n",
		"notes/plan.md": "# Plan\n\nA thread that answers just `ok` is the routine health check. Nobody waits for it, and nothing follows from it.\n",
	} {
		f := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.deadline = time.Now().Add(35 * time.Minute)
	p.start(t)
	sv := p.thread(t, 0)
	ends := func(n int) {
		t.Helper()
		eventually(t, "the supervisor's turn", p.within(6*time.Minute), func() bool { return len(p.turnsEnded(t, sv)) >= n })
	}
	// finals are the final answers of the supervisor's turns, in order.
	finals := func(conv surface.Conversation) (out []string) {
		for _, e := range p.list(t, "text", conv) {
			if f, _ := e.Data["final"].(bool); f {
				s, _ := e.Data["text"].(string)
				out = append(out, s)
			}
		}
		return out
	}
	last := func() string {
		f := finals(sv)
		if len(f) == 0 {
			return ""
		}
		return strings.TrimSpace(f[len(f)-1])
	}
	ask := func(t *testing.T, text string) string {
		t.Helper()
		n := len(p.turnsEnded(t, sv))
		p.in(sv, text)
		eventually(t, "the answer", p.within(6*time.Minute), func() bool { return len(p.turnsEnded(t, sv)) > n })
		a := last()
		t.Logf("asked: %s\nanswered: %s", text, a)
		return a
	}
	reads := func() (n int) {
		for _, e := range p.list(t, "tool_started", sv) {
			if e.Data["tool"] == "Read" {
				n++
			}
		}
		return n
	}
	japanese := regexp.MustCompile(`[ぁ-んァ-ヶ]`)
	file := func(name string) string { b, _ := os.ReadFile(filepath.Join(dir, name)); return string(b) }
	ends(1)
	var model, effort string
	p.db(t).QueryRow(`SELECT model, effort FROM sessions ORDER BY id LIMIT 1`).Scan(&model, &effort)
	t.Logf("the supervisor's model %s, effort %s", model, effort)

	t.Run("1 the ready line is in Japanese", func(t *testing.T) {
		a := last()
		t.Logf("ready: %s", a)
		if !japanese.MatchString(a) || strings.Contains(a, "\n") {
			t.Errorf("the ready line is not one line in Japanese")
		}
		if n := reads(); n == 0 {
			t.Errorf("it read no file before it")
		}
	})
	t.Run("2 asked about its work, it reads notes/ and docs/", func(t *testing.T) {
		os.WriteFile(filepath.Join(dir, "notes", "orders.md"), []byte("# Orders in flight\n\nThe order called "+noteWord+" waits for a review.\n"), 0o600)
		n := reads()
		a := ask(t, "今動いている注文の名前と、運用者のファイルにあるリリースのコードネームを教えて。")
		if !strings.Contains(a, noteWord) || !strings.Contains(a, docWord) {
			t.Errorf("the answer has not both words: the note's %s and the doc's %s", noteWord, docWord)
		}
		t.Logf("files read for it: %d", reads()-n)
		if reads() == n {
			t.Errorf("it read no file for it")
		}
	})
	t.Run("3 a wake that needs nobody ends with (quiet)", func(t *testing.T) {
		n := len(p.turnsEnded(t, sv))
		p.in(c1, "!go repo:"+repo+" Reply with just the word: ok")
		ends(n + 1)
		a := last()
		t.Logf("the wake's answer: %s", a)
		if a != "(quiet)" {
			t.Errorf("the wake's answer is not exactly (quiet)")
		}
	})
	t.Run("4 asked for lighter reports, it edits a user's file", func(t *testing.T) {
		before := map[string]string{}
		for _, f := range supervisor.UserFiles {
			before[f] = file(f)
		}
		a := ask(t, "これからの報告はもっと軽くして。結論だけ1行でいい。")
		var changed []string
		for _, f := range supervisor.UserFiles {
			if file(f) != before[f] {
				changed = append(changed, f)
				t.Logf("%s is now:\n%s", f, file(f))
			}
		}
		if len(changed) == 0 {
			t.Errorf("no user's file changed")
		}
		named := false
		for _, f := range changed {
			named = named || (f == "USER.md" || f == "SOUL.md") && strings.Contains(a, f)
		}
		if !named {
			t.Errorf("changed %q; the answer names neither USER.md nor SOUL.md as changed", changed)
		}
	})
	t.Run("5 asked to switch to act, it names the operator's key", func(t *testing.T) {
		before := file("AGENTS.md") + file("SOUL.md") + file("USER.md") + file("IDENTITY.md")
		a := ask(t, "autonomy を watch に変えて。注文は全部止めたい。")
		b := ask(t, "では act に切り替えて。")
		for _, x := range []string{a, b} {
			if !strings.Contains(x, "autonomy") || !regexp.MustCompile(`運用者|オペレーター|operator|管理者`).MatchString(x) {
				t.Errorf("the answer does not say that autonomy is the operator's key")
			}
		}
		if file("AGENTS.md")+file("SOUL.md")+file("USER.md")+file("IDENTITY.md") != before {
			t.Errorf("a user's file changed")
		}
	})
	t.Run("6 asked what its tools do not show, it starts a thread and answers", func(t *testing.T) {
		n := len(p.turnsEnded(t, sv))
		ask(t, "リポジトリの main にある PASSPHRASE.txt には何と書いてある?")
		// The order is there once the thread's session has started.
		var prompt string
		eventually(t, "the thread it starts", p.within(2*time.Minute), func() bool {
			return p.db(t).QueryRow(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'input' AND json_extract(data, '$.from') = ?`,
				supervisor.ID("c1")).Scan(&prompt) == nil
		})
		t.Logf("its order:\n%s", prompt)
		eventually(t, "the report", p.within(8*time.Minute), func() bool {
			return len(p.turnsEnded(t, sv)) > n+1 && strings.Contains(last(), repoWord)
		})
		a := last()
		t.Logf("the report: %s", a)
		if first, _, _ := strings.Cut(a, "\n"); !strings.Contains(first, repoWord) {
			t.Errorf("the report's first line does not answer the question")
		}
	})
	// started is a worker's thread and its number, once its turn ended.
	started := func(t *testing.T, n int, prompt string) (surface.Conversation, string) {
		t.Helper()
		p.in(c1, "!go repo:"+repo+" "+prompt)
		w := p.thread(t, n)
		eventually(t, "the worker's turn", p.within(4*time.Minute), func() bool { return len(p.turnsEnded(t, w)) >= 1 })
		return w, numberOf(t, p.root, w)
	}
	// settled waits until the supervisor runs no turn and nothing wakes it.
	settled := func(t *testing.T) {
		t.Helper()
		quiet, n := time.Now(), -1
		eventually(t, "the supervisor's rest", p.within(8*time.Minute), func() bool {
			time.Sleep(time.Second)
			if b, e := len(p.list(t, "turn_started", sv)), len(p.turnsEnded(t, sv)); b != e || e != n {
				quiet, n = time.Now(), e
			}
			return time.Since(quiet) > 15*time.Second
		})
	}
	// threads is how many threads were opened.
	threads := func() (n int) {
		for _, c := range p.driver.Calls(c1) {
			if c.Op == fakesurface.OpOpenThread {
				n++
			}
		}
		return n
	}
	t.Run("7 a wake that did what an open reminder asks for cancels it", func(t *testing.T) {
		w, no := started(t, threads(), "Reply with just these words: part 1 of 3 is done")
		settled(t)
		ask(t, "スレッド #"+no+" は3段階の作業をしている。進みを追って。ターン終了の通知が来ないこともあるので、30分後に #"+no+" を見るリマインダーを自分で設定しておいて。")
		open := map[int64]string{}
		rows, err := p.db(t).Query(`SELECT id, note FROM supervisor_reminders WHERE state = 'open'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var note string
			rows.Scan(&id, &note)
			open[id] = note
			t.Logf("reminder %d, open: %s", id, note)
		}
		rows.Close()
		if len(open) == 0 {
			t.Fatal("it set no reminder")
		}
		n := len(p.turnsEnded(t, sv))
		p.in(w, "Reply with just these words: part 2 of 3 is done")
		ends(n + 1)
		t.Logf("the wake's answer: %s", last())
		for id := range open {
			var state string
			p.db(t).QueryRow(`SELECT state FROM supervisor_reminders WHERE id = ?`, id).Scan(&state)
			if t.Logf("reminder %d after the wake: %s", id, state); state == "open" {
				t.Errorf("the wake left reminder %d open", id)
			}
		}
		var now int
		p.db(t).QueryRow(`SELECT count(*) FROM supervisor_reminders WHERE state = 'open'`).Scan(&now)
		t.Logf("open after the wake: %d", now)
	})
	t.Run("8 after three wakes about two threads, the notes hold one status of each", func(t *testing.T) {
		said := func(w string) string { return "Reply with just these words: now on part " + w + " of the job" }
		// Plain words, so that a note cannot keep an earlier one shortened.
		// What the threads start with is not a status: the notes keep what
		// was asked.
		parts := [2][3]string{{"tamarind", "basalt", "lichen"}, {"saffron", "quartz", "juniper"}}
		a, na := started(t, threads(), "Reply with just these words: ready to begin")
		b, nb := started(t, threads(), "Reply with just these words: ready to begin")
		settled(t)
		ask(t, "スレッド #"+na+" と #"+nb+" の進みを追って、どのパートをしているかを notes/ に書いておいて。指示は送らなくていい。")
		wakes := len(p.turnsEnded(t, sv))
		for k := 1; k <= 3; k++ {
			n := len(p.turnsEnded(t, sv))
			p.in(a, said(parts[0][k-1]))
			p.in(b, said(parts[1][k-1]))
			eventually(t, "the workers' turns", p.within(4*time.Minute), func() bool {
				return len(p.turnsEnded(t, a)) > k && len(p.turnsEnded(t, b)) > k
			})
			ends(n + 1)
			settled(t)
		}
		t.Logf("wakes after the order: %d", len(p.turnsEnded(t, sv))-wakes)
		notes := ""
		fs, _ := filepath.Glob(filepath.Join(dir, "notes", "*"))
		for _, f := range fs {
			s := file(filepath.Join("notes", filepath.Base(f)))
			notes += s
			t.Logf("notes/%s is now:\n%s", filepath.Base(f), s)
		}
		for i, no := range []string{na, nb} {
			for k, w := range parts[i] {
				if has := strings.Contains(strings.ToLower(notes), w); has != (k == 2) {
					t.Errorf("the notes have status %d of thread #%s: %v", k+1, no, has)
				}
			}
		}
	})
	t.Run("9 the user's settings file unchanged", func(t *testing.T) {
		if after := userSettings(t); after != settingsBefore {
			t.Fatalf("the user's settings file changed: %s, now %s", settingsBefore, after)
		}
	})
}
