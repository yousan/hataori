package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/chattext"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

type chatServiceFunc func(context.Context, control.LinkRef, control.ReadChatRequest, int) (ChatPage, error)

func (f chatServiceFunc) ReadChat(ctx context.Context, l control.LinkRef, q control.ReadChatRequest, n int) (ChatPage, error) {
	return f(ctx, l, q, n)
}

func chatPolicy(mode string) chattext.Policy {
	return chattext.Policy{Scope: "scope", Autonomy: mode, People: people.New([]people.Person{{Name: "owner", IDs: []string{"chat:1"}}}, "owner", nil)}
}

var chatOwn = control.LinkRef{Client: "chat", Channel: "channel"}

func TestChatReadRefusesBeforeServiceWithoutScopeOrAuthority(t *testing.T) {
	ctx := context.Background()
	calls := 0
	svc := chatServiceFunc(func(context.Context, control.LinkRef, control.ReadChatRequest, int) (ChatPage, error) {
		calls++
		return ChatPage{}, nil
	})
	commit := func(context.Context, []chattext.Reference) error { return nil }
	q := control.ReadChatRequest{Op: "read", Thread: "thread"}
	for _, api := range []control.API{
		ChatReads(nil, chatOwn, nil, chatPolicy(Act), nil, commit),
		ChatReads(nil, chatOwn, nil, chatPolicy(Act), svc, nil),
		ChatReads(nil, control.LinkRef{}, nil, chatPolicy(Act), svc, commit),
	} {
		if _, err := api.ReadChat(ctx, q); !errors.Is(err, control.ErrNotSupported) {
			t.Fatalf("missing barrier: %v", err)
		}
	}
	api := ChatReads(nil, chatOwn, nil, chatPolicy(Act), svc, commit)
	for _, channel := range []string{"chat:other", "other:channel", "chat:dm"} {
		q.Channel = channel
		if _, err := api.ReadChat(ctx, q); !errors.Is(err, control.ErrForbidden) {
			t.Fatalf("%s: %v", channel, err)
		}
	}
	if calls != 0 {
		t.Fatalf("unauthorized service reads: %d", calls)
	}
}

func TestChatReadRefusesPrivateDMWrongParentAndForeignMessage(t *testing.T) {
	for _, what := range []string{"private", "dm", "wrong-parent", "foreign-client", "foreign-message", "over-limit"} {
		t.Run(what, func(t *testing.T) {
			committed := false
			svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, _ control.ReadChatRequest, n int) (ChatPage, error) {
				if n != 50 {
					t.Fatalf("limit=%d", n)
				}
				page := ChatPage{Link: l, Public: true}
				switch what {
				case "private", "dm":
					page.Public = false
				case "wrong-parent":
					page.Link.Channel = "other"
				case "foreign-client":
					page.Link.Client = "other"
				case "foreign-message":
					page.Messages = []ChatMessage{{Message: chattext.Message{Client: l.Client}, Link: control.LinkRef{Client: l.Client, Channel: "other", Thread: l.Thread}}}
				case "over-limit":
					page.Messages = make([]ChatMessage, 51)
				}
				return page, nil
			})
			api := ChatReads(nil, chatOwn, nil, chatPolicy(Propose), svc, func(context.Context, []chattext.Reference) error { committed = true; return nil })
			out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread"})
			if !errors.Is(err, control.ErrForbidden) || out.Text != "" || committed {
				t.Fatalf("%+v %v commit=%v", out, err, committed)
			}
		})
	}
}

func TestChatReadMarksBeforeAnswerAndFailsWithoutCommittedMark(t *testing.T) {
	for _, mode := range []string{Watch, Propose, Act} {
		for _, fail := range []bool{false, true} {
			calls := 0
			svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, _ control.ReadChatRequest, _ int) (ChatPage, error) {
				return ChatPage{Link: l, Public: true, Before: "older", After: "newer", Messages: []ChatMessage{{Link: l, Message: chattext.Message{Client: l.Client, ID: "post", Author: "guest", Kind: "person", Text: "UNTRUSTED-BODY"}, FileNames: []string{"picture.png"}}}}, nil
			})
			barrier := errors.New("effective scope mark unavailable")
			api := ChatReads(nil, chatOwn, nil, chatPolicy(mode), svc, func(_ context.Context, sources []chattext.Reference) error {
				calls++
				want := 0
				if mode != Act {
					want = 1
				}
				if len(sources) != want {
					t.Fatalf("%s sources=%+v", mode, sources)
				}
				if fail {
					return barrier
				}
				return nil
			})
			out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread", Before: "older-page"})
			if calls != 1 {
				t.Fatal("barrier not called once")
			}
			if fail {
				if !errors.Is(err, barrier) || out.Text != "" {
					t.Fatalf("mark failure leaked: %+v %v", out, err)
				}
				continue
			}
			if err != nil || strings.Contains(out.Text, "UNTRUSTED-BODY") != (mode != Act) || out.Before != "older" || out.After != "newer" {
				t.Fatalf("%s: %+v %v", mode, out, err)
			}
			if mode != Act && !strings.Contains(out.Text, "> File (not fetched): picture.png") {
				t.Fatal(out.Text)
			}
		}
	}
}

func TestChatReadExplicitChannelTreatsOwnerTextAsOutside(t *testing.T) {
	svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, q control.ReadChatRequest, _ int) (ChatPage, error) {
		if l.Client != "other" || l.Channel != "public" || q.After != "cursor" {
			t.Fatalf("%+v %+v", l, q)
		}
		return ChatPage{Link: l, Public: true, Messages: []ChatMessage{{Link: l, Message: chattext.Message{Client: l.Client, ID: "post", Author: "1", Kind: "person", Text: "FOREIGN-OWNER"}}}}, nil
	})
	p := chatPolicy(Act)
	p.Trusted = []string{"other:1"}
	api := ChatReads(nil, chatOwn, []string{"other:public"}, p, svc, func(context.Context, []chattext.Reference) error { return nil })
	out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Channel: "other:public", Thread: "thread", After: "cursor"})
	if err != nil || strings.Contains(out.Text, "FOREIGN-OWNER") {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestChatListFiltersTitlesAndRejectsForeignPrivateEntries(t *testing.T) {
	for _, mode := range []string{Act, Propose} {
		for _, bad := range []string{"", "foreign", "private"} {
			marked := false
			svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, q control.ReadChatRequest, n int) (ChatPage, error) {
				if !q.Archived || q.Before != "cursor" || n != 50 {
					t.Fatalf("%+v", q)
				}
				a, b := l, l
				a.Thread = "a"
				b.Thread = "b"
				threads := []ChatThread{{Link: a, Public: true, Name: "GUEST-TITLE"}, {Link: b, Public: true, Known: true, Name: "daemon title"}}
				if bad == "foreign" {
					threads[0].Link.Channel = "elsewhere"
				}
				if bad == "private" {
					threads[0].Public = false
				}
				return ChatPage{Link: l, Public: true, Threads: threads, Next: "next"}, nil
			})
			api := ChatReads(nil, chatOwn, nil, chatPolicy(mode), svc, func(_ context.Context, s []chattext.Reference) error { marked = len(s) > 0; return nil })
			out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "list", Archived: true, Before: "cursor"})
			if bad != "" {
				if !errors.Is(err, control.ErrForbidden) || out.Text != "" || marked {
					t.Fatalf("%s %+v %v", bad, out, err)
				}
				continue
			}
			if err != nil || strings.Contains(out.Text, "GUEST-TITLE") != (mode == Propose) || marked != (mode == Propose) || !strings.Contains(out.Text, "daemon title") || out.Next != "next" {
				t.Fatalf("%s %+v %v mark=%v", mode, out, err, marked)
			}
		}
	}
}

func TestChatServiceErrorsAreSurfacedAndNotEmptyHistory(t *testing.T) {
	serviceErr := errors.New("HTTP 403 service code 50013: missing permissions")
	api := ChatReads(nil, chatOwn, nil, chatPolicy(Act), chatServiceFunc(func(context.Context, control.LinkRef, control.ReadChatRequest, int) (ChatPage, error) {
		return ChatPage{}, serviceErr
	}), func(context.Context, []chattext.Reference) error { t.Fatal("committed failed read"); return nil })
	out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread"})
	if !errors.Is(err, serviceErr) || !strings.Contains(err.Error(), "HTTP 403") || out.Text != "" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestChatReadKeepsChronologyAndEveryMessageBehindCursor(t *testing.T) {
	for _, after := range []string{"", "start"} {
		svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, _ control.ReadChatRequest, _ int) (ChatPage, error) {
			page := ChatPage{Link: l, Public: true, Before: "older", After: "newer"}
			for i := 0; i < 50; i++ {
				id := fmt.Sprintf("%02d", i)
				author := "guest"
				if i%2 == 0 {
					author = "1"
				}
				page.Messages = append(page.Messages, ChatMessage{Link: l, Message: chattext.Message{Client: l.Client, ID: id, Author: author, Kind: "person", Text: strings.Repeat("歴", 1024)}})
			}
			return page, nil
		})
		api := ChatReads(nil, chatOwn, nil, chatPolicy(Propose), svc, func(context.Context, []chattext.Reference) error { return nil })
		out, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread", After: after})
		if err != nil || len(out.Text) > chattext.MaxBytes || !strings.Contains(out.Text, "[text cut;") {
			t.Fatalf("%v %d bytes", err, len(out.Text))
		}
		previous := -1
		for i := 0; i < 50; i++ {
			n := i
			if after == "" {
				n = 49 - i
			}
			where := strings.Index(out.Text, fmt.Sprintf("Message %q", fmt.Sprintf("chat:%02d", n)))
			if where < 0 || where <= previous {
				t.Fatalf("lost or reordered message %d, after=%q", n, after)
			}
			previous = where
		}
	}
}

func TestChatReadLimitValidationBeforeService(t *testing.T) {
	calls := 0
	svc := chatServiceFunc(func(_ context.Context, l control.LinkRef, _ control.ReadChatRequest, n int) (ChatPage, error) {
		calls++
		if n != 1 {
			t.Fatal(n)
		}
		return ChatPage{Link: l, Public: true}, nil
	})
	api := ChatReads(nil, chatOwn, nil, chatPolicy(Act), svc, func(context.Context, []chattext.Reference) error { return nil })
	for _, n := range []int{-1, 51} {
		if _, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread", Limit: n}); err == nil {
			t.Fatal(n)
		}
	}
	if calls != 0 {
		t.Fatal("invalid limit reached service")
	}
	if _, err := api.ReadChat(context.Background(), control.ReadChatRequest{Op: "read", Thread: "thread", Limit: 1}); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
