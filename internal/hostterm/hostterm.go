// Package hostterm lists the terminals on the host that hataori does not
// own, reads the screen of a tmux pane, and types into a tmux pane that the
// owner operates (design, section 7.2).
//
// It runs a fixed list of commands. Listing and reading change nothing:
// tmux's list-panes and capture-pane, and ps. Typing goes only through
// tmux: send-keys with a key name from a fixed list, and load-buffer with
// the text on standard input into a buffer of the operation's own, then
// paste-buffer, which deletes it; delete-buffer and list-buffers remove
// such a buffer after a failure and one left over from an earlier run. It
// opens no terminal and reads nothing under /proc, so the same code runs
// on Linux and macOS.
package hostterm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// command is one of the commands this package runs. Its argv is fixed but
// for what is appended: a pane id to capture-pane, and the buffer, the
// pane and a key name to the input commands (input.go).
type command struct {
	name string
	args []string
}

// paneFormat is what list-panes prints for each pane, one tab-separated
// line. tmux prints a tab in a window's name or a command as it is, so every
// text field has its tabs replaced with spaces; the directory is also last.
const paneFormat = "#{pane_id}\t#{s/\t/ /:session_name}\t#{window_index}\t#{s/\t/ /:window_name}\t#{pane_index}\t" +
	"#{pane_tty}\t#{s/\t/ /:pane_current_command}\t#{session_attached}\t#{window_activity}\t#{s/\t/ /:pane_current_path}"

// The commands. Commands is every one of them; a test checks that nothing
// else runs.
var (
	psCmd          = command{"ps", []string{"-A", "-o", "pid=,ppid=,pgid=,tpgid=,tty=,comm="}}
	listPanesCmd   = command{"tmux", []string{"list-panes", "-a", "-F", paneFormat}}
	capturePaneCmd = command{"tmux", []string{"capture-pane", "-p", "-t"}} // then the pane id
	Commands       = []command{psCmd, listPanesCmd, capturePaneCmd, sendKeysCmd, loadBufferCmd, pasteBufferCmd, deleteBufferCmd, listBuffersCmd}
)

// Lister lists and reads host terminals. The zero value uses the default
// tmux server.
type Lister struct {
	// Tmux comes before every tmux subcommand, such as "-L", "name" to
	// reach a server on a socket of its own.
	Tmux []string
	// Own is the command name of hataori's own processes. A process of
	// that name without a terminal, such as a session host that has not
	// been recorded yet, counts as owned too.
	Own string

	// Buffers starts the name of every tmux buffer an operation of this
	// instance uses, such as "hataori-input-<instance>-". Only a buffer
	// whose name starts with it is ever deleted. Empty: Operate refuses.
	Buffers string

	// run runs one command with stdin on its standard input and returns
	// its standard output; nil runs it.
	run func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

func (l *Lister) exec(ctx context.Context, c command, extra ...string) ([]byte, error) {
	return l.execIn(ctx, nil, c, extra...)
}

// execIn runs c with stdin on its standard input.
func (l *Lister) execIn(ctx context.Context, stdin []byte, c command, extra ...string) ([]byte, error) {
	args := slices.Concat(c.args, extra)
	if c.name == "tmux" {
		args = slices.Concat(l.Tmux, args)
	}
	if l.run != nil {
		return l.run(ctx, stdin, c.name, args...)
	}
	cmd := exec.CommandContext(ctx, c.name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hostterm: %s %s: %w: %s", c.name, c.args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// List returns the terminals on the host, the default tmux server's panes
// (or those of the server Tmux names) first, leaving out
// every terminal that a process under one of owned uses: the session hosts'
// own terminals. Without a tmux server, or without tmux, there are no panes.
func (l *Lister) List(ctx context.Context, owned []int) ([]control.HostTerminal, error) {
	// The panes first: every process of a listed pane then is in the
	// process table read after, so none escapes the check for owned ones.
	panes, err := l.panes(ctx)
	if err != nil {
		return nil, err
	}
	out, err := l.exec(ctx, psCmd)
	if err != nil {
		return nil, err
	}
	procs := parsePS(out)
	hidden := ownedDevices(procs, slices.Concat(owned, ownPIDs(procs, l.Own)))
	list := []control.HostTerminal{} // an empty list, not null, on the wire
	inTmux := map[string]bool{}
	for _, p := range panes {
		inTmux[p.Device] = true
		if !hidden[p.Device] {
			list = append(list, p)
		}
	}
	for _, t := range others(procs) {
		if !hidden[t.Device] && !inTmux[t.Device] {
			list = append(list, t)
		}
	}
	return list, nil
}

// panes lists the tmux server's panes. A host with no server, or no tmux,
// has none.
func (l *Lister) panes(ctx context.Context) ([]control.HostTerminal, error) {
	if l.run == nil {
		if _, err := exec.LookPath("tmux"); err != nil {
			return nil, nil
		}
	}
	out, err := l.exec(ctx, listPanesCmd)
	if err != nil {
		if noServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return parsePanes(out), nil
}

// noServer reports whether a tmux error says that no server runs.
func noServer(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no server running") || strings.Contains(s, "error connecting to") ||
		strings.Contains(s, "No such file or directory")
}

var paneID = regexp.MustCompile(`^%[0-9]+$`)

// Screen returns the visible screen of one tmux pane, by its id ("%3").
// ok is false for a pane that List would not return, which is not read.
func (l *Lister) Screen(ctx context.Context, pane string, owned []int) (screen string, ok bool, err error) {
	if !paneID.MatchString(pane) {
		return "", false, nil
	}
	list, err := l.List(ctx, owned)
	if err != nil {
		return "", false, err
	}
	if !slices.ContainsFunc(list, func(t control.HostTerminal) bool { return t.Tmux != nil && t.Tmux.Pane == pane }) {
		return "", false, nil
	}
	out, err := l.exec(ctx, capturePaneCmd, pane)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// parsePanes reads list-panes output in paneFormat.
func parsePanes(out []byte) []control.HostTerminal {
	var list []control.HostTerminal
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, "\t", 10)
		if len(f) != 10 || !paneID.MatchString(f[0]) {
			continue
		}
		t := control.HostTerminal{
			Device:  f[5],
			Process: f[6],
			Tmux: &control.TmuxPane{Pane: f[0], Session: f[1], Window: f[2], WindowName: f[3], Index: f[4],
				Dir: f[9], Attached: f[7] != "" && f[7] != "0"},
		}
		if sec, err := strconv.ParseInt(f[8], 10, 64); err == nil && sec > 0 {
			at := time.Unix(sec, 0).UTC()
			t.Activity = &at
		}
		list = append(list, t)
	}
	return list
}

// proc is one line of ps.
type proc struct {
	pid, ppid, pgid, tpgid int
	tty                    string // the device, or "" for none
	comm                   string
}

// parsePS reads the output of psCmd. A process without a terminal has an
// empty tty.
func parsePS(out []byte) []proc {
	var ps []proc
	for _, line := range strings.Split(string(out), "\n") {
		f, rest := fields(line, 5)
		if len(f) < 5 {
			continue
		}
		var n [4]int
		ok := true
		for i := range n {
			v, err := strconv.Atoi(f[i])
			ok = ok && err == nil
			n[i] = v
		}
		if !ok {
			continue
		}
		p := proc{pid: n[0], ppid: n[1], pgid: n[2], tpgid: n[3]}
		if tty := f[4]; !strings.HasPrefix(tty, "?") && tty != "-" {
			p.tty = "/dev/" + tty
		}
		// The command is the rest of the line, and may hold spaces. macOS
		// prints the executable's path.
		if rest = strings.TrimSpace(rest); rest != "" {
			p.comm = filepath.Base(rest)
		}
		ps = append(ps, p)
	}
	return ps
}

// fields splits off the first n space-separated fields of line and returns
// them with the rest of the line.
func fields(line string, n int) ([]string, string) {
	var f []string
	for len(f) < n {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			break
		}
		end := strings.IndexAny(line, " \t")
		if end < 0 {
			end = len(line)
		}
		f = append(f, line[:end])
		line = line[end:]
	}
	return f, line
}

// ownPIDs are the processes named own that hold no terminal. Linux cuts a
// command name to 15 bytes.
func ownPIDs(ps []proc, own string) []int {
	if own == "" {
		return nil
	}
	var pids []int
	for _, p := range ps {
		if p.tty == "" && (p.comm == own || len(p.comm) == 15 && strings.HasPrefix(own, p.comm)) {
			pids = append(pids, p.pid)
		}
	}
	return pids
}

// ownedDevices is the set of terminals used by a process in owned or under
// one.
func ownedDevices(ps []proc, owned []int) map[string]bool {
	hidden := map[string]bool{}
	if len(owned) == 0 {
		return hidden
	}
	parent := map[int]int{}
	for _, p := range ps {
		parent[p.pid] = p.ppid
	}
	under := func(pid int) bool {
		for range len(ps) + 1 { // a loop in the table ends here
			if slices.Contains(owned, pid) {
				return true
			}
			next, ok := parent[pid]
			if !ok || next == pid || next <= 0 {
				return false
			}
			pid = next
		}
		return false
	}
	for _, p := range ps {
		if p.tty != "" && under(p.pid) {
			hidden[p.tty] = true
		}
	}
	return hidden
}

// others is one HostTerminal per terminal device in ps, with the command of
// its foreground process group: the group's leader, or else its first
// process.
func others(ps []proc) []control.HostTerminal {
	byTTY := map[string][]proc{}
	var order []string
	for _, p := range ps {
		if p.tty == "" {
			continue
		}
		if _, seen := byTTY[p.tty]; !seen {
			order = append(order, p.tty)
		}
		byTTY[p.tty] = append(byTTY[p.tty], p)
	}
	slices.Sort(order)
	var list []control.HostTerminal
	for _, dev := range order {
		var fg *proc
		for i, p := range byTTY[dev] {
			if p.pgid != p.tpgid {
				continue
			}
			if fg == nil || p.pid == p.pgid {
				fg = &byTTY[dev][i]
			}
			if p.pid == p.pgid {
				break
			}
		}
		t := control.HostTerminal{Device: dev}
		if fg != nil {
			t.Process = fg.comm
		}
		list = append(list, t)
	}
	return list
}
