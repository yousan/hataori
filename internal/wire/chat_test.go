package wire

import (
	"context"
	"errors"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

type chatTokenAPI struct {
	control.API
	asked []control.ReadChatRequest
}

func (a *chatTokenAPI) ReadChat(_ context.Context, q control.ReadChatRequest) (control.ReadChatAnswer, error) {
	a.asked = append(a.asked, q)
	return control.ReadChatAnswer{Text: "> past decision\n", Before: "older"}, nil
}

func TestReadChatTransportRequiresSpecializedTokenAPI(t *testing.T) {
	root := t.TempDir()
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	a := &chatTokenAPI{API: owner}
	token, err := srv.Issue(a)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	q := control.ReadChatRequest{Op: "read", Thread: "service-thread", Limit: 1}
	if _, err := plain.ReadChat(context.Background(), q); !errors.Is(err, control.ErrNotSupported) {
		t.Fatalf("owner got %v", err)
	}
	if len(a.asked) != 0 {
		t.Fatal("owner reached token API")
	}
	limited, err := DialToken(root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	out, err := limited.ReadChat(context.Background(), q)
	if err != nil || out.Text != "> past decision\n" || out.Before != "older" || len(a.asked) != 1 || a.asked[0] != q {
		t.Fatalf("%+v %v %+v", out, err, a.asked)
	}
}
