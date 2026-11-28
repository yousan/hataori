package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// This child is an isolated transport/process fixture, not provider evidence.
// It writes no login files, starts no session host, and receives no ambient env.
func TestWorkerLoginProcessHelper(t *testing.T) {
	var args []string
	for i, s := range os.Args {
		if s == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	// Only the disposable helper opts into argv observation. Both guard
	// and CLI record their real arguments; the persistence scan checks them.
	if capture := os.Getenv("WORKER_LOGIN_ARGV_CAPTURE"); capture != "" {
		b, err := json.Marshal(os.Args)
		if err != nil {
			os.Exit(1)
		}
		f, err := os.OpenFile(capture, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(1)
		}
		_, err = f.Write(append(b, '\n'))
		f.Close()
		if err != nil {
			os.Exit(1)
		}
	}
	if args[0] == "guard" {
		os.Exit(LoginGuardMain(args[1:]))
	}
	if os.Getenv("WORKER_LOGIN_HELPER") != "1" {
		return
	}
	switch args[0] {
	case "empty-env-owner":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd, err := startLoginCommand(ctx, agent.Launch{Argv: []string{"/bin/sh", "-c", `test -z "$WORKER_LOGIN_FORBIDDEN"`}}, os.Getenv("WORKER_LOGIN_DIR"), helperArgv("guard"), io.Discard)
		if err != nil || !cmd.wait(ctx) {
			os.Exit(1)
		}
	case "owned-owner":
		a := &loginTestAdapter{mode: "lifetime", extraEnv: []string{"WORKER_LOGIN_LIFETIME=" + os.Getenv("WORKER_LOGIN_LIFETIME")}}
		f, err := startLoginAttempt(context.Background(), a, agent.Spec{Dir: os.Getenv("WORKER_LOGIN_DIR")}, helperArgv("guard"), 30*time.Second)
		if err != nil {
			os.Exit(1)
		}
		fmt.Println("ready")
		<-f.done
	case "lifetime", "detached-lifetime":
		if args[0] == "detached-lifetime" {
			if _, err := syscall.Setsid(); err != nil {
				os.Exit(1)
			}
		}
		c, err := net.Dial("tcp", os.Getenv("WORKER_LOGIN_LIFETIME"))
		if err != nil {
			os.Exit(1)
		}
		defer c.Close()
		fmt.Fprintln(c, "alive")
		c.Read(make([]byte, 1))
	case "status-good":
	case "status-bad":
		os.Exit(1)
	case "status-failed-claims":
		fmt.Println(`{"loggedIn":true}`)
		os.Exit(1)
	case "status-flood":
		fmt.Print(strings.Repeat("x", loginLineLimit+1))
	case "status-hang":
		for {
			time.Sleep(time.Hour)
		}
	case "capture":
		file, err := os.Open(os.Getenv("WORKER_LOGIN_CAPTURE"))
		if err != nil {
			os.Exit(1)
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var row struct {
				Line string `json:"line"`
			}
			if json.Unmarshal(scanner.Bytes(), &row) != nil {
				os.Exit(1)
			}
			fmt.Println(row.Line)
		}
		if scanner.Err() != nil {
			os.Exit(1)
		}
	case "held":
		c, err := net.Dial("tcp", os.Getenv("WORKER_LOGIN_LIFETIME"))
		if err != nil {
			os.Exit(1)
		}
		defer c.Close()
		fmt.Fprintln(c, "alive")
		fmt.Println("ready")
		c.Read(make([]byte, 1)) // fixture cleanup can end it by closing this socket
	case "descendant":
		cmd := exec.Command(helperArgv("held")[0], helperArgv("held")[1:]...)
		cmd.Env = os.Environ()
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			os.Exit(1)
		}
		if !bufio.NewScanner(out).Scan() {
			os.Exit(1)
		}
		// Deliberately leave the owned descendant and inherited stderr alive.
		os.Exit(0)
	case "code", "quit", "failure":
		fmt.Println("link https://login.example.invalid/marker")
		fmt.Print("code:") // the prompt is deliberately unterminated
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() || scanner.Text() != "marker-secret" {
			os.Exit(1)
		}
		if args[0] == "failure" {
			os.Exit(1)
		}
	case "device":
		fmt.Println("link https://login.example.invalid/device")
		fmt.Println("device marker-device")
		if address := os.Getenv("WORKER_LOGIN_DEVICE_RELEASE"); address != "" {
			c, err := net.Dial("tcp", address)
			if err != nil {
				os.Exit(1)
			}
			defer c.Close()
			c.Read(make([]byte, 1)) // fixture's external device-auth completion
		}
	case "flood":
		fmt.Print(strings.Repeat("x", loginLineLimit+1))
		os.Stdin.Read(make([]byte, 1))
	case "updates":
		for range 100 {
			fmt.Println("link https://login.example.invalid/flood")
		}
		os.Stdin.Read(make([]byte, 1))
	case "wait":
		fmt.Println("link https://login.example.invalid/wait")
		os.Stdin.Read(make([]byte, 1))
	default:
		os.Exit(64)
	}
	os.Exit(0)
}

type loginTestAdapter struct {
	agent.Adapter
	mode       string
	health     agent.Health
	mu         sync.Mutex
	probes     []agent.Spec
	checks     []agent.Spec
	launch     agent.Spec
	extraEnv   []string
	statusMode string
}

func (a *loginTestAdapter) ForWorker() agent.Adapter { return a }

func helperArgv(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestWorkerLoginProcessHelper$", "--", mode}
}

func (a *loginTestAdapter) LoginLaunch(spec agent.Spec) (agent.Launch, error) {
	a.launch = spec
	return agent.Launch{Argv: helperArgv(a.mode), Env: append([]string{"WORKER_LOGIN_HELPER=1", "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0"}, a.extraEnv...)}, nil
}

func (a *loginTestAdapter) LoginDecode(b []byte) agent.LoginStep {
	s := string(b)
	if link, ok := strings.CutPrefix(s, "link "); ok {
		return agent.LoginStep{Link: link}
	}
	if code, ok := strings.CutPrefix(s, "device "); ok {
		return agent.LoginStep{ShowCode: code}
	}
	if s == "code:" {
		return agent.LoginStep{WantsCode: true}
	}
	return agent.LoginStep{}
}

func (a *loginTestAdapter) LoginCode(s string) ([]byte, error) {
	if a.mode == "device" {
		return nil, agent.ErrNoCodeBack
	}
	return []byte(s + "\n"), nil
}

func (a *loginTestAdapter) Probe(ctx context.Context, spec agent.Spec) (agent.Health, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.probes = append(a.probes, spec)
	return a.health, nil
}

func (a *loginTestAdapter) LoginStatusLaunch(spec agent.Spec) (agent.Launch, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks = append(a.checks, spec)
	mode := "status-bad"
	if a.health.Installed && a.health.LoggedIn && !a.health.Unknown {
		mode = "status-good"
	}
	if a.statusMode != "" {
		mode = a.statusMode
	}
	return agent.Launch{Argv: helperArgv(mode), Env: append([]string{"WORKER_LOGIN_HELPER=1", "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0"}, a.extraEnv...)}, nil
}

func loginFixture(t *testing.T, mode string, health agent.Health, timeout time.Duration) (*loginAttempt, *loginTestAdapter) {
	t.Helper()
	a := &loginTestAdapter{mode: mode, health: health}
	spec := agent.Spec{Dir: t.TempDir(), ConfigDir: filepath.Join(t.TempDir(), "disposable-login")}
	f, err := startLoginAttempt(context.Background(), a, spec, helperArgv("guard"), timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.stop)
	return f, a
}

func nextLoginUpdate(t *testing.T, f *loginAttempt) loginUpdate {
	t.Helper()
	select {
	case u, ok := <-f.updates:
		if !ok {
			t.Fatal("login updates ended unexpectedly")
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("login update timed out")
		return loginUpdate{}
	}
}

func TestWorkerLoginCleanExitAndSameConfigurationProbe(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		health agent.Health
		want   bool
	}{
		{"code", agent.Health{Installed: true, LoggedIn: true}, true},
		{"quit", agent.Health{Installed: true}, false},
		{"failure", agent.Health{Installed: true, LoggedIn: true}, false},
		{"code", agent.Health{Installed: true, LoggedIn: true, Unknown: true}, false},
		{"code", agent.Health{LoggedIn: true}, false},
	} {
		t.Run(fmt.Sprintf("%s/%+v", tc.mode, tc.health), func(t *testing.T) {
			f, a := loginFixture(t, tc.mode, tc.health, 5*time.Second)
			for {
				u := nextLoginUpdate(t, f)
				if u.Step.WantsCode {
					if err := f.code("marker-secret"); err != nil {
						t.Fatal(err)
					}
				}
				if !u.Done {
					continue
				}
				if u.LoggedIn != tc.want {
					t.Fatalf("logged in = %v, want %v", u.LoggedIn, tc.want)
				}
				<-f.done
				a.mu.Lock()
				defer a.mu.Unlock()
				if tc.mode == "failure" {
					if len(a.checks) != 0 {
						t.Fatal("failed command was probed")
					}
				} else if len(a.checks) != 1 || a.checks[0].ConfigDir != a.launch.ConfigDir || a.checks[0].Dir != a.launch.Dir {
					t.Fatal("probe did not use the login configuration")
				}
				if err := f.code("marker-secret"); err == nil {
					t.Fatal("ended command accepted a code")
				}
				return
			}
		})
	}
}

func TestWorkerLoginCancellationAndBounds(t *testing.T) {
	for _, mode := range []string{"wait", "flood", "updates"} {
		t.Run(mode, func(t *testing.T) {
			f, a := loginFixture(t, mode, agent.Health{Installed: true, LoggedIn: true}, 200*time.Millisecond)
			select {
			case <-f.done:
			case <-time.After(5 * time.Second):
				t.Fatal("bounded attempt did not exit without a consumer")
			}
			for u := range f.updates {
				if u.LoggedIn {
					t.Fatal("cancelled/oversized flow reported success")
				}
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.checks) != 0 {
				t.Fatal("cancelled attempt checked status")
			}
		})
	}
}

func TestWorkerLoginRejectsInputOutsideWaitingCommand(t *testing.T) {
	f, _ := loginFixture(t, "wait", agent.Health{}, 5*time.Second)
	nextLoginUpdate(t, f)
	for _, code := range []string{"", "marker-secret", "two words", "line\ncode", strings.Repeat("x", loginLineLimit+1)} {
		if err := f.code(code); err == nil {
			t.Fatal("command that never requested input accepted it")
		}
	}
	f.stop()
	f, _ = loginFixture(t, "device", agent.Health{Installed: true, LoggedIn: true}, 5*time.Second)
	for u := range f.updates {
		if !u.NoCode {
			t.Fatal("device flow was not identified")
		}
		if err := f.code("marker-secret"); err == nil {
			t.Fatal("device flow accepted code-back input")
		}
	}
}
