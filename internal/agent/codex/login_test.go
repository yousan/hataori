package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func TestLoginDecodeCapture(t *testing.T) {
	b, err := os.ReadFile("testdata/0.163.0/login_device/notes.json")
	if err != nil {
		t.Fatal(err)
	}
	var notes struct {
		CodeShape string `json:"code_shape"`
	}
	if err = json.Unmarshal(b, &notes); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/0.163.0/login_device/output.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a := &Adapter{}
	links, codes := 0, 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var row struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		got := a.LoginDecode([]byte(row.Line))
		switch got {
		case agent.LoginStep{Link: "https://auth.openai.com/codex/device"}:
			links++
		case agent.LoginStep{ShowCode: notes.CodeShape}:
			codes++
		case agent.LoginStep{}:
		default:
			t.Errorf("unexpected step for %q: %+v", row.Line, got)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if links != 1 || codes != 1 {
		t.Fatalf("links %d, codes %d", links, codes)
	}
	// The end was not captured; cli/src/login.rs finish_login prints these lines.
	for _, tc := range []struct {
		line string
		want agent.LoginStep
	}{{"Successfully logged in", agent.LoginStep{}}, {"Error logging in: device auth timed out after 15 minutes", agent.LoginStep{Refused: "Error logging in: device auth timed out after 15 minutes"}}} {
		if got := a.LoginDecode([]byte(tc.line)); got != tc.want {
			t.Errorf("%q: %+v", tc.line, got)
		}
	}
}

func TestLoginLaunchAndCode(t *testing.T) {
	x, err := New([]byte(`{"command":"custom","pass_env":["SECRET"]}`))
	if err != nil {
		t.Fatal(err)
	}
	a := x.(*Adapter)
	a.environ = func() []string {
		return []string{"PATH=p", "CODEX_HOME=h", "SECRET=s", "OPENAI_API_KEY=x", "TERM=x", "GIT_DIR=x"}
	}
	l, err := a.LoginLaunch(agent.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Argv, []string{"custom", "login", "--device-auth"}) || !reflect.DeepEqual(l.Env, []string{"PATH=p", "CODEX_HOME=h", "SECRET=s"}) || l.Terminal || len(l.Files) != 0 {
		t.Fatalf("launch: %+v", l)
	}
	for _, code := range []string{"", "ABCD"} {
		b, err := a.LoginCode(code)
		if b != nil || !errors.Is(err, agent.ErrNoCodeBack) {
			t.Errorf("%q: %q %v", code, b, err)
		}
	}
}
