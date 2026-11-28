package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func TestWorkerLoginUsesSelectedConfiguration(t *testing.T) {
	x, err := New([]byte(`{"command":"codex-disposable","pass_env":["MAIN_ONLY"]}`))
	if err != nil {
		t.Fatal(err)
	}
	a := x.(*Adapter)
	a.environ = func() []string {
		return []string{"HOME=/fixture/home", "CODEX_HOME=/fixture/default", "PATH=/fixture/bin", "MAIN_ONLY=forbidden"}
	}
	w := a.ForWorker().(*Adapter)
	spec := agent.Spec{ConfigDir: "/fixture/selected"}
	l, err := w.LoginLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Argv, []string{"codex-disposable", "login", "--device-auth"}) {
		t.Fatal("login command changed")
	}
	count := 0
	for _, kv := range l.Env {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			count++
			if kv != "CODEX_HOME="+spec.ConfigDir {
				t.Fatal("login used another configuration")
			}
		}
		if strings.HasPrefix(kv, "MAIN_ONLY=") {
			t.Fatal("worker inherited ambient pass_env")
		}
	}
	if count != 1 {
		t.Fatal("login did not select exactly one configuration")
	}
	status, err := w.LoginStatusLaunch(spec)
	if err != nil || !slices.Equal(status.Argv, []string{"codex-disposable", "login", "status"}) || !slices.Equal(status.Env, l.Env) {
		t.Fatal("status did not use the selected CLI/configuration")
	}
}
