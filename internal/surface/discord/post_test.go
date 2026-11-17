package discord

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
)

// content returns the JSON payload of a request, from a JSON body or from
// the payload_json part of a multipart one, and the names of its files.
func content(t *testing.T, r request) (map[string]any, []string) {
	t.Helper()
	var m map[string]any
	mt, params, _ := mime.ParseMediaType(r.contentType)
	if mt != "multipart/form-data" {
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatalf("%s: %v", r.key, err)
		}
		return m, nil
	}
	var files []string
	mr := multipart.NewReader(strings.NewReader(string(r.body)), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		if p.FormName() == "payload_json" {
			json.NewDecoder(p).Decode(&m)
		} else {
			files = append(files, p.FileName())
		}
	}
	return m, files
}

func TestOpenThread(t *testing.T) {
	r := newFakeREST(t)
	from := payload(t, "bot/rest-thread-from-message.json")
	ch, msg := id(from["parent_id"]), id(from["id"]) // the thread has its message's id
	r.serve(t, "POST /channels/"+ch+"/messages/"+msg+"/threads", "bot/rest-thread-from-message.json")
	d := testDriver(t, botID(t), r, nil)
	conv, err := d.OpenThread(context.Background(), surface.Conversation{Channel: ch, Thread: msg}, strings.Repeat("あ", 150))
	if err != nil || conv != (surface.Conversation{Channel: ch, Thread: id(from["id"])}) {
		t.Fatalf("OpenThread: %+v, %v", conv, err)
	}
	body, _ := content(t, r.requests("POST /channels/" + ch + "/messages/" + msg + "/threads")[0])
	if n := len([]rune(body["name"].(string))); n != 100 {
		t.Fatalf("title of %d characters", n)
	}

	without := payload(t, "second/rest-thread-without-message.json")
	r.serve(t, "POST /channels/"+ch+"/threads", "second/rest-thread-without-message.json")
	conv, err = d.OpenThread(context.Background(), surface.Conversation{Channel: ch}, "a title")
	if err != nil || conv != (surface.Conversation{Channel: ch, Thread: id(without["id"])}) {
		t.Fatalf("OpenThread without a message: %+v, %v", conv, err)
	}
	body, _ = content(t, r.requests("POST /channels/" + ch + "/threads")[0])
	if body["type"] != 11.0 || body["name"] != "a title" {
		t.Fatalf("body %v", body)
	}
}

// Opening a thread again from a message that has one is refused; the
// driver returns the thread the message names (captured 2026-11-17).
func TestOpenThreadAgainFromTheSameMessage(t *testing.T) {
	r := newFakeREST(t)
	first := payload(t, "rethread/rest-rethread-first.json")
	msg := payload(t, "rethread/rest-rethread-get-message.json")
	ch, m := id(msg["channel_id"]), id(msg["id"])
	r.serve(t, "POST /channels/"+ch+"/messages/"+m+"/threads", "rethread/rest-rethread-again.json")
	r.serve(t, "GET /channels/"+ch+"/messages/"+m, "rethread/rest-rethread-get-message.json")
	d := testDriver(t, botID(t), r, nil)
	conv, err := d.OpenThread(context.Background(), surface.Conversation{Channel: ch, Thread: m}, "again")
	if err != nil || conv != (surface.Conversation{Channel: ch, Thread: id(first["id"])}) {
		t.Fatalf("OpenThread: %+v, %v; want the thread %s opened the first time", conv, err, id(first["id"]))
	}
	if n := r.called("POST /channels/"); n != 1 {
		t.Fatalf("%d thread requests, want 1", n)
	}
}

func TestSplit(t *testing.T) {
	long := strings.Repeat("word ", 900) // 4500 characters
	parts := split(long, 2000)
	if len(parts) != 3 || strings.Join(parts, "") != long {
		t.Fatalf("%d parts", len(parts))
	}
	for _, p := range parts {
		if length(p) > 2000 {
			t.Fatalf("part of %d", length(p))
		}
	}

	// A code block cut by the split is closed, then opened again.
	code := "before\n```go\n" + strings.Repeat("x := 1\n", 400) + "```\nafter"
	parts = split(code, 2000)
	if len(parts) < 2 {
		t.Fatalf("%d parts", len(parts))
	}
	if !strings.HasSuffix(parts[0], "\n```") || !strings.HasPrefix(parts[1], "```go\n") {
		t.Fatalf("fence not closed and reopened:\n%q\n%q", parts[0][len(parts[0])-20:], parts[1][:20])
	}
	for i, p := range parts {
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d has an unclosed fence", i)
		}
		if length(p) > 2000 {
			t.Errorf("part %d has %d characters", i, length(p))
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1], "```\nafter") {
		t.Fatal("the end is lost")
	}

	// A long opening line of a code block does not crowd out the text.
	for _, text := range []string{
		"```" + strings.Repeat("a", 1993) + "\n" + strings.Repeat("x", 10) + "\n```",
		"```" + strings.Repeat("a", 3000) + "\n```",
	} {
		for i, p := range split(text, 2000) {
			if length(p) > 2000 {
				t.Errorf("part %d of a long opening line has %d characters", i, length(p))
			}
		}
	}

	// Nothing is dropped, not even indentation inside code.
	indented := "```go\n" + strings.Repeat(" ", 1200) + strings.Repeat("x", 1000) + "\n```"
	if got := strings.Count(strings.Join(split(indented, 2000), ""), " "); got != 1200 {
		t.Errorf("%d of 1200 spaces kept", got)
	}
	prose := strings.Repeat("line one\nline two and more ", 300)
	if strings.Join(split(prose, 2000), "") != prose {
		t.Error("prose changed by the split")
	}

	// Characters outside the BMP count twice.
	emoji := strings.Repeat("😀", 1500)
	for _, p := range split(emoji, 2000) {
		if length(p) > 2000 || !strings.HasPrefix(p, "😀") {
			t.Fatalf("emoji part of %d", length(p))
		}
	}
}

func TestPostSplitsAndAttachesToLast(t *testing.T) {
	r := newFakeREST(t)
	th := id(payload(t, "bot/rest-post-thread-1.json")["channel_id"])
	key := "POST /channels/" + th + "/messages"
	for i := 1; i <= 3; i++ {
		r.serve(t, key, "bot/rest-post-thread-"+string(rune('0'+i))+".json")
	}
	dir := t.TempDir()
	var files []surface.File
	for _, n := range []string{"a.txt", "b.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600)
		files = append(files, surface.File{Name: n, Path: filepath.Join(dir, n)})
	}
	d := testDriver(t, botID(t), r, nil)
	text := "start\n```sh\n" + strings.Repeat("echo hello\n", 400) + "```\nend"
	ref, err := d.Post(context.Background(), surface.Conversation{Channel: "1", Thread: th}, "event-7", surface.Message{Text: text, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != id(payload(t, "bot/rest-post-thread-1.json")["id"]) {
		t.Fatalf("returned %s, not the first message", ref.ID)
	}
	reqs := r.requests(key)
	if len(reqs) != 3 {
		t.Fatalf("%d messages", len(reqs))
	}
	nonces := map[string]bool{}
	var joined []string
	for i, q := range reqs {
		body, names := content(t, q)
		c := body["content"].(string)
		joined = append(joined, c)
		if length(c) > 2000 {
			t.Errorf("message %d has %d characters", i, length(c))
		}
		if (i == len(reqs)-1) != (len(names) == 2) {
			t.Errorf("message %d has files %v", i, names)
		}
		n, _ := body["nonce"].(string)
		if len(n) == 0 || len(n) > 25 || body["enforce_nonce"] != true || nonces[n] {
			t.Errorf("message %d: nonce %q enforce %v", i, n, body["enforce_nonce"])
		}
		nonces[n] = true
	}
	if !strings.HasPrefix(joined[0], "start\n```sh\n") || !strings.HasPrefix(joined[1], "```sh\n") || !strings.HasSuffix(joined[2], "```\nend") {
		t.Fatal("parts out of order or fences lost")
	}
}

// A file that cannot be read fails the post before anything is posted, and
// a retry would not help.
func TestPostChecksFilesFirst(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	long := strings.Repeat("word ", 900)
	for _, f := range []surface.File{{Name: "dir", Path: t.TempDir()}, {Name: "gone", Path: filepath.Join(t.TempDir(), "missing")}} {
		_, err := d.Post(context.Background(), surface.Conversation{Channel: "1", Thread: "2"}, "", surface.Message{Text: long, Files: []surface.File{f}})
		var e *Error
		if !errors.As(err, &e) || e.Temporary() {
			t.Errorf("%s: %v", f.Name, err)
		}
	}
	if n := r.called("POST "); n != 0 {
		t.Fatalf("%d messages posted before the file was checked", n)
	}
}

// The same key sends the same nonce, and the service answers with the
// first message (the two captures).
func TestPostKeyIsTheNonce(t *testing.T) {
	r := newFakeREST(t)
	first := payload(t, "bot/rest-post-nonce-1.json")
	key := "POST /channels/" + id(first["channel_id"]) + "/messages"
	r.serve(t, key, "bot/rest-post-nonce-1.json")
	r.serve(t, key, "bot/rest-post-nonce-2.json")
	d := testDriver(t, botID(t), r, nil)
	conv := surface.Conversation{Channel: "1", Thread: id(first["channel_id"])}
	a, err1 := d.Post(context.Background(), conv, "k", surface.Message{Text: "x"})
	b, err2 := d.Post(context.Background(), conv, "k", surface.Message{Text: "x"})
	if err1 != nil || err2 != nil || a != b || a.ID != id(first["id"]) {
		t.Fatalf("%v %v %v %v", a, b, err1, err2)
	}
	q := r.requests(key)
	n1, _ := content(t, q[0])
	n2, _ := content(t, q[1])
	if n1["nonce"] != n2["nonce"] {
		t.Fatal("the same key sent different nonces")
	}
}

func TestRateLimitWaitsOnlyItsRoute(t *testing.T) {
	r := newFakeREST(t)
	limited := capture(t, "second/rest-rate-limited.json")
	th := strings.Split(limited["request"].(map[string]any)["path"].(string), "/")[2]
	r.serve(t, "POST /channels/"+th+"/messages", "second/rest-rate-limited.json")
	r.serve(t, "POST /channels/"+th+"/messages", "second/rest-post-thread.json")
	other := id(payload(t, "bot/rest-post-channel.json")["channel_id"])
	r.serve(t, "POST /channels/"+other+"/messages", "bot/rest-post-channel.json")
	wait := limited["headers"].(map[string]any)["retry-after"].(string) // what the service said
	d := testDriver(t, botID(t), r, nil)

	start := time.Now()
	done := make(chan error, 1)
	var ref surface.MessageRef
	go func() {
		var err error
		ref, err = d.Post(context.Background(), surface.Conversation{Channel: "1", Thread: th}, "", surface.Message{Text: "x"})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := d.Post(context.Background(), surface.Conversation{Channel: other}, "", surface.Message{Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the other route waited %v", el)
	}
	select {
	case err := <-done:
		t.Fatalf("the limited post did not wait: %v", err)
	default:
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 4500*time.Millisecond {
		t.Fatalf("retried after %v; the service said %s seconds", el, wait)
	}
	if ref.ID != id(payload(t, "second/rest-post-thread.json")["id"]) || len(r.requests("POST /channels/"+th+"/messages")) != 2 {
		t.Fatalf("ref %v", ref)
	}
}

func TestEditDeleteSetThread(t *testing.T) {
	r := newFakeREST(t)
	edit := capture(t, "bot/rest-edit.json")
	editPath := edit["request"].(map[string]any)["path"].(string)
	r.serve(t, "PATCH "+editPath, "bot/rest-edit.json")
	parts := strings.Split(editPath, "/") // /channels/<thread>/messages/<id>
	d := testDriver(t, botID(t), r, nil)
	conv := surface.Conversation{Channel: "1", Thread: parts[2]}
	if err := d.Edit(context.Background(), surface.MessageRef{Conv: conv, ID: parts[4]}, surface.Message{Text: "edited"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := content(t, r.requests("PATCH " + editPath)[0]); b["content"] != "edited" {
		t.Fatalf("edit body %v", b)
	}

	del := capture(t, "bot/rest-delete.json")["request"].(map[string]any)["path"].(string)
	r.serve(t, "DELETE "+del, "bot/rest-delete.json")
	r.serve(t, "DELETE "+del, "bot/rest-delete-again.json")
	dp := strings.Split(del, "/")
	ref := surface.MessageRef{Conv: surface.Conversation{Channel: "1", Thread: dp[2]}, ID: dp[4]}
	if err := d.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(context.Background(), ref); err != nil {
		t.Fatalf("deleting a deleted message: %v", err)
	}

	// Refusals say that a retry cannot help.
	r.serve(t, "PATCH "+editPath, "bot/rest-delete-again.json") // the message is gone
	err := d.Edit(context.Background(), surface.MessageRef{Conv: conv, ID: parts[4]}, surface.Message{Text: "x"})
	var e *Error
	if !errors.As(err, &e) || e.Temporary() || e.Status != 404 {
		t.Fatalf("edit of a deleted message: %v", err)
	}
	r.serve(t, "POST /channels/"+parts[2]+"/messages", "bot/rest-post-too-long.json")
	_, err = d.Post(context.Background(), conv, "", surface.Message{Text: "x"})
	if !errors.As(err, &e) || e.Temporary() || e.Code != 50035 {
		t.Fatalf("refused post: %v", err)
	}
	if err := d.Edit(context.Background(), surface.MessageRef{Conv: conv, ID: parts[4]}, surface.Message{Text: strings.Repeat("x", 2001)}); !errors.As(err, &e) || e.Temporary() {
		t.Fatalf("too long an edit: %v", err)
	}
	if !(&Error{Status: 429}).Temporary() || !(&Error{Status: 502}).Temporary() || !(&Error{Err: errors.New("reset")}).Temporary() {
		t.Fatal("a rate limit, a server error or no response would not be retried")
	}

	title := capture(t, "bot/rest-set-thread-title.json")["request"].(map[string]any)["path"].(string)
	r.serve(t, "PATCH "+title, "bot/rest-set-thread-title.json")
	r.serve(t, "PATCH "+title, "second/rest-set-thread-archived.json")
	r.serve(t, "PATCH "+title, "second/rest-delete-thread-refused.json")
	tc := surface.Conversation{Channel: "1", Thread: strings.Split(title, "/")[2]}
	if err := d.SetThread(context.Background(), tc, surface.ThreadMeta{Title: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThread(context.Background(), tc, surface.ThreadMeta{Archived: true}); err != nil {
		t.Fatal(err)
	}
	q := r.requests("PATCH " + title)
	b0, _ := content(t, q[0])
	b1, _ := content(t, q[1])
	if b0["name"] != "renamed" || b0["archived"] != false || b1["archived"] != true || b1["name"] != nil {
		t.Fatalf("bodies %v %v", b0, b1)
	}
	if err := d.SetThread(context.Background(), tc, surface.ThreadMeta{Title: "x"}); !errors.As(err, &e) || e.Status != 403 || e.Temporary() {
		t.Fatalf("refused: %v", err)
	}
}
