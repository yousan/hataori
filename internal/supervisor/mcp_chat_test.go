package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

type chatAPI struct {
	control.API
	asked []control.ReadChatRequest
	err   error
}

func (a *chatAPI) ReadChat(_ context.Context, q control.ReadChatRequest) (control.ReadChatAnswer, error) {
	a.asked = append(a.asked, q)
	return control.ReadChatAnswer{Text: "> old decision\n", Next: "next"}, a.err
}

func TestChatToolsUseReadChatAndKeepServiceIds(t *testing.T) {
	a := &chatAPI{}
	m := &MCP{Dial: func() (control.API, error) { return a, nil }}
	for _, tc := range []struct{ name, args, op, thread string }{
		{ToolChatList, `{"channel":"chat:channel","archived":true,"before":"cursor"}`, "list", ""},
		{ToolChatRead, `{"thread":"service-thread","after":"cursor"}`, "read", "service-thread"},
	} {
		text, bad := call(t, m, tc.name, tc.args)
		q := a.asked[len(a.asked)-1]
		if bad || q.Op != tc.op || q.Thread != tc.thread || !strings.Contains(text, "old decision") {
			t.Fatalf("%+v %s", q, text)
		}
	}
	a.err = errors.New("service missing permission")
	if text, bad := call(t, m, ToolChatRead, `{"thread":"thread"}`); !bad || !strings.Contains(text, "missing permission") {
		t.Fatal(text)
	}
	before := len(a.asked)
	for _, args := range []string{`{}`, `{"thread":"t","scope":"foreign"}`, `{"thread":7}`, `{"thread":"t"} {}`} {
		if _, bad := call(t, m, ToolChatRead, args); !bad {
			t.Fatal(args)
		}
	}
	if len(a.asked) != before {
		t.Fatal("invalid arguments reached API")
	}
}
