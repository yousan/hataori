package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/codex"
)

type capturedLoginAdapter struct {
	*loginTestAdapter
	decoder agent.LoginFlow
}

type loginWithoutStatus struct {
	agent.Adapter
	flow agent.LoginFlow
}

func (a *loginWithoutStatus) LoginLaunch(s agent.Spec) (agent.Launch, error) {
	return a.flow.LoginLaunch(s)
}
func (a *loginWithoutStatus) LoginDecode(b []byte) agent.LoginStep { return a.flow.LoginDecode(b) }
func (a *loginWithoutStatus) LoginCode(s string) ([]byte, error)   { return a.flow.LoginCode(s) }

func TestWorkerLoginRefusesAdapterWithoutBoundedStatus(t *testing.T) {
	a := &loginTestAdapter{mode: "wait"}
	f, err := startLoginAttempt(context.Background(), &loginWithoutStatus{Adapter: a, flow: a}, agent.Spec{Dir: t.TempDir()}, helperArgv("guard"), 5*time.Second)
	if err == nil {
		f.stop()
		t.Fatal("adapter without bounded status was admitted")
	}
}

func (a *capturedLoginAdapter) LoginDecode(b []byte) agent.LoginStep { return a.decoder.LoginDecode(b) }
func (a *capturedLoginAdapter) LoginCode(string) ([]byte, error)     { return nil, agent.ErrNoCodeBack }

func TestWorkerLoginCapturedDevicePrompt(t *testing.T) {
	path, err := filepath.Abs("../agent/codex/testdata/0.163.0/login_device")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(path, "notes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var notes struct {
		Link string `json:"link_shape"`
		Code string `json:"code_shape"`
	}
	if json.Unmarshal(b, &notes) != nil {
		t.Fatal("invalid capture notes")
	}
	x, err := codex.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &capturedLoginAdapter{loginTestAdapter: &loginTestAdapter{mode: "capture", extraEnv: []string{"WORKER_LOGIN_CAPTURE=" + filepath.Join(path, "output.jsonl")}}, decoder: x.(agent.LoginFlow)}
	f, err := startLoginAttempt(context.Background(), a, agent.Spec{Dir: t.TempDir()}, helperArgv("guard"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	links, codes := 0, 0
	for u := range f.updates {
		if !u.NoCode {
			t.Fatal("captured device flow requested code-back")
		}
		if u.Step.Link == notes.Link {
			links++
		}
		if u.Step.ShowCode == notes.Code {
			codes++
		}
		if f.code("marker-secret") == nil {
			t.Fatal("captured device flow accepted owner input")
		}
		if u.LoggedIn {
			t.Fatal("prompt-only capture was treated as provider success")
		}
	}
	if links != 1 || codes != 1 {
		t.Fatal("real captured link/code did not survive the bounded pipe reader")
	}
}

func TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly(t *testing.T) {
	for _, mode := range []string{"status-failed-claims", "status-flood", "status-hang"} {
		t.Run(mode, func(t *testing.T) {
			a := &loginTestAdapter{mode: "device", health: agent.Health{Installed: true, LoggedIn: true}, statusMode: mode}
			f, err := startLoginAttempt(context.Background(), a, agent.Spec{Dir: t.TempDir()}, helperArgv("guard"), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer f.stop()
			select {
			case <-f.done:
			case <-time.After(4 * time.Second):
				t.Fatal("status command exceeded its enforced deadline")
			}
			for u := range f.updates {
				if u.LoggedIn {
					t.Fatal("failed/unbounded status command certified login")
				}
			}
		})
	}
}
