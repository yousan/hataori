package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// The status line. Claude Code gives the command of the statusLine setting
// its session's state as JSON on stdin, rate_limits among it (design,
// section 7.2; docs/notes/agent-usage-limits.md). hataori's settings set
// that command to `hataori statusline`, which replaces the user's status
// line, so the command runs the user's own one with the same input and
// prints what it printed: the user's line still shows in hataori's panes.
// It hands rate_limits to the session host as a hook call of the event
// statusLineEvent, when they changed or every limitsEvery.
const (
	statusLineEvent = "hataori:statusline"
	// limitsEvery is how often the status line hands on rate_limits that
	// did not change, and so the Every of its Limits events.
	limitsEvery = 5 * time.Minute
	// statusLineRefresh is the refreshInterval hataori sets, in seconds,
	// unless the user's status line asks for a shorter one: the command runs
	// at least that often, so a report is never more than limitsEvery late.
	statusLineRefresh = 60
	// statusLineLast is the file in the state directory that holds what
	// was last handed on, and when.
	statusLineLast = "claude-statusline-last.json"
)

// userLineTimeout bounds the user's status line command, and everything it
// started: on the deadline its process group is killed.
var userLineTimeout = 10 * time.Second

// statusLine is the statusLine setting.
type statusLine struct {
	Type            string `json:"type"`
	Command         string `json:"command"`
	Padding         *int   `json:"padding,omitempty"`
	RefreshInterval int    `json:"refreshInterval,omitempty"` // seconds
}

// userStatusLine returns the status line the user would have without
// hataori's --settings: from the settings files Claude Code reads below the
// command line, highest first (the project's local and shared settings in
// the workspace, then the user's own), the first that sets one. It only
// reads them. Managed settings rank above --settings, so a status line set
// there replaces hataori's and needs nothing here.
func (a *Adapter) userStatusLine(dir string) (statusLine, bool) {
	var files []string
	if dir != "" {
		files = append(files, filepath.Join(dir, ".claude", "settings.local.json"), filepath.Join(dir, ".claude", "settings.json"))
	}
	if d := a.configDir(); d != "" {
		files = append(files, filepath.Join(d, "settings.json"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			StatusLine *statusLine `json:"statusLine"`
		}
		if json.Unmarshal(b, &s) != nil || s.StatusLine == nil {
			continue
		}
		if s.StatusLine.Type == "command" && s.StatusLine.Command != "" {
			return *s.StatusLine, true
		}
		return statusLine{}, false // set there, but to no command: nothing shows
	}
	return statusLine{}, false
}

// configDir is Claude Code's configuration directory in the CLI's
// environment.
func (a *Adapter) configDir() string {
	var home string
	for _, kv := range a.environ() {
		k, v, _ := bytes.Cut([]byte(kv), []byte("="))
		switch string(k) {
		case "CLAUDE_CONFIG_DIR":
			if len(v) > 0 {
				return string(v)
			}
		case "HOME":
			home = string(v)
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// statusLineSetting is hataori's statusLine for a session in dir whose
// state directory is stateDir.
func (a *Adapter) statusLineSetting(dir, stateDir string) statusLine {
	cmd := shellQuote(a.hataori) + " statusline --last " + shellQuote(filepath.Join(stateDir, statusLineLast))
	s := statusLine{Type: "command", RefreshInterval: statusLineRefresh}
	if u, ok := a.userStatusLine(dir); ok {
		cmd += " --run " + shellQuote(u.Command)
		s.Padding = u.Padding
		if u.RefreshInterval > 0 && u.RefreshInterval < s.RefreshInterval {
			s.RefreshInterval = u.RefreshInterval
		}
	}
	s.Command = cmd
	return s
}

// StatusLineUsageText documents `hataori statusline`.
const StatusLineUsageText = `usage: hataori statusline --last FILE [--run COMMAND]

Claude Code's status line in a hataori session. Reads the status line's
input on stdin, hands its rate_limits to the session host named by
$` + host.HookEnvVar + ` when they changed or every 5 minutes (FILE keeps what
was handed on last), runs COMMAND with sh and the same input, and prints
what it printed. Exit status 0, or 64 for bad arguments.
`

// StatusLineMain is `hataori statusline`. Nothing it fails to do stops
// the user's line from showing.
func StatusLineMain(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	last := fs.String("last", "", "")
	run := fs.String("run", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *last == "" {
		fmt.Fprint(stderr, StatusLineUsageText)
		return 64
	}
	input, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	line := make(chan []byte, 1)
	go func() { line <- userLine(*run, input) }()
	if socket := getenv(host.HookEnvVar); socket != "" {
		handOn(socket, *last, input, time.Now())
	}
	stdout.Write(<-line)
	return 0
}

// userLine runs the user's status line command with input and returns
// what it printed.
func userLine(command string, input []byte) []byte {
	if command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), userLineTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(input)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second // a descendant that keeps stdout open
	out, _ := cmd.Output()
	return out
}

// lastHandedOn is the content of the --last file.
type lastHandedOn struct {
	At      int64           `json:"at"` // Unix ms
	Payload json.RawMessage `json:"payload"`
}

// handOn hands the input's rate_limits to the host, unless the same was
// handed on less than limitsEvery ago. An input without rate_limits hands
// on nothing: the account reports none (or none yet).
func handOn(socket, last string, input []byte, now time.Time) {
	var in struct {
		RateLimits json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(input, &in) != nil || len(in.RateLimits) == 0 || string(in.RateLimits) == "null" {
		return
	}
	var compact bytes.Buffer
	if json.Compact(&compact, in.RateLimits) != nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"hook_event_name": statusLineEvent, "rate_limits": json.RawMessage(compact.Bytes())})
	var prev lastHandedOn
	if b, err := os.ReadFile(last); err == nil && json.Unmarshal(b, &prev) == nil &&
		bytes.Equal(prev.Payload, payload) && now.Sub(time.UnixMilli(prev.At)) < limitsEvery {
		return
	}
	if _, err := host.Call(socket, payload, false, 5*time.Second); err != nil {
		return
	}
	// Two runs at once may both hand on the same value: one more line in
	// the host's log, read as the same report. Each writes a file of its
	// own and renames it, so the file is always one whole record.
	b, _ := json.Marshal(lastHandedOn{At: now.UnixMilli(), Payload: payload})
	f, err := os.CreateTemp(filepath.Dir(last), filepath.Base(last)+".*")
	if err != nil {
		return
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil && cerr == nil {
		err = os.Rename(f.Name(), last)
	}
	if err != nil {
		os.Remove(f.Name())
	}
}

// windowMinutes are the lengths of the windows Claude Code names.
var windowMinutes = map[string]int64{"five_hour": 300, "seven_day": 10080}

// decodeLimits reads the rate_limits a status line call handed on, taken
// by the host at at. Windows come in the order five_hour, seven_day, then
// any other by name; one without used_percentage is left out.
func decodeLimits(raw json.RawMessage, at time.Time) *agent.LimitReport {
	var ws map[string]struct {
		Used   *float64 `json:"used_percentage"`
		Resets *float64 `json:"resets_at"`
	}
	if json.Unmarshal(raw, &ws) != nil {
		return nil
	}
	names := make([]string, 0, len(ws))
	for n := range ws {
		names = append(names, n)
	}
	rank := func(n string) int {
		switch n {
		case "five_hour":
			return 0
		case "seven_day":
			return 1
		}
		return 2
	}
	slices.SortFunc(names, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	r := &agent.LimitReport{At: at, Every: limitsEvery, Windows: []agent.LimitWindow{}}
	for _, n := range names {
		w := ws[n]
		if w.Used == nil {
			continue
		}
		lw := agent.LimitWindow{Name: n, Minutes: windowMinutes[n], Used: int(math.Round(*w.Used))}
		if w.Resets != nil && *w.Resets > 0 {
			lw.Resets = time.Unix(int64(*w.Resets), 0)
		}
		r.Windows = append(r.Windows, lw)
	}
	return r
}
