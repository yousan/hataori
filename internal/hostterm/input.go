package hostterm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/yousan/hataori/internal/control"
)

// The commands that type into a pane, and those that remove a buffer of
// an operation (design, section 7.2). Text is only ever on load-buffer's
// standard input, never in an argument.
var (
	sendKeysCmd     = command{"tmux", []string{"send-keys", "-t"}}                      // then the pane and one key name
	loadBufferCmd   = command{"tmux", []string{"load-buffer", "-b"}}                    // then the buffer and "-"; the text on standard input
	pasteBufferCmd  = command{"tmux", []string{"paste-buffer", "-d", "-b"}}             // then the buffer, "-t", the pane, and "-p" for a paste
	deleteBufferCmd = command{"tmux", []string{"delete-buffer", "-b"}}                  // then the buffer
	listBuffersCmd  = command{"tmux", []string{"list-buffers", "-F", "#{buffer_name}"}} // the buffers' names, one per line
)

// bufferSuffix is what follows Buffers in an operation's buffer's name.
var bufferSuffix = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ownBuffer reports whether name is a buffer of an operation of this
// instance.
func (l *Lister) ownBuffer(name string) bool {
	rest, ok := strings.CutPrefix(name, l.Buffers)
	return l.Buffers != "" && ok && bufferSuffix.MatchString(rest)
}

// NewBuffer returns the name of a tmux buffer for one operation: Buffers
// and 16 random hexadecimal digits.
func (l *Lister) NewBuffer() (string, error) {
	if l.Buffers == "" {
		return "", errors.New("hostterm: no buffer prefix: host input is not set up")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("hostterm: %w", err)
	}
	return l.Buffers + hex.EncodeToString(b[:]), nil
}

// matchesOwn reports whether comm, a command name as ps or tmux reports
// it, is own. Linux cuts a command name to 15 bytes.
func matchesOwn(comm, own string) bool {
	return own != "" && (comm == own || len(comm) == 15 && strings.HasPrefix(own, comm))
}

// Check looks at one tmux pane as tmux lists it now. p is nil when tmux
// does not list it. own is true when one of hataori's processes runs
// there: the command tmux reports for the pane is Own, or, with procs, a
// process named Own, or one under owned or under a process named Own
// without a terminal (a session host), holds the pane's terminal.
func (l *Lister) Check(ctx context.Context, pane string, owned []int, procs bool) (p *control.TmuxPane, own bool, err error) {
	if !paneID.MatchString(pane) {
		return nil, false, nil
	}
	panes, err := l.panes(ctx)
	if err != nil {
		return nil, false, err
	}
	var t *control.HostTerminal
	for i := range panes {
		if panes[i].Tmux.Pane == pane {
			t = &panes[i]
		}
	}
	if t == nil {
		return nil, false, nil
	}
	own = matchesOwn(t.Process, l.Own)
	if procs && !own {
		out, err := l.exec(ctx, psCmd)
		if err != nil {
			return nil, false, err
		}
		ps := parsePS(out)
		for _, pr := range ps {
			if pr.tty == t.Device && matchesOwn(pr.comm, l.Own) {
				own = true
			}
		}
		own = own || ownedDevices(ps, append(append([]int{}, owned...), ownPIDs(ps, l.Own)...))[t.Device]
	}
	return t.Tmux, own, nil
}

// Keys types keys into pane, in order: a key by send-keys with its name,
// a text by load-buffer into buffer from standard input, then
// paste-buffer -d of that buffer into the pane (with -p for a paste),
// which deletes it. A pane that is not a pane id, a buffer not of this
// instance, an empty or ambiguous key and a key name that
// control.ValidHostKey refuses refuse the whole batch before anything
// runs. When loading or pasting fails, the buffer is deleted. No error
// holds a key or a text.
func (l *Lister) Keys(ctx context.Context, pane, buffer string, keys []control.HostKey) error {
	if !paneID.MatchString(pane) {
		return errors.New("hostterm: not a tmux pane id")
	}
	if !l.ownBuffer(buffer) {
		return errors.New("hostterm: not a buffer of this instance's host input")
	}
	for i, k := range keys {
		switch {
		case k.Key != "" && (k.Text != "" || k.Paste):
			return fmt.Errorf("hostterm: input %d of the batch has both a key and a text", i+1)
		case k.Key != "" && !control.ValidHostKey(k.Key):
			return fmt.Errorf("hostterm: input %d of the batch is not a key name hataori sends", i+1)
		case k.Key == "" && k.Text == "":
			return fmt.Errorf("hostterm: input %d of the batch is empty", i+1)
		}
	}
	for _, k := range keys {
		if k.Key != "" {
			if _, err := l.exec(ctx, sendKeysCmd, pane, k.Key); err != nil {
				return err
			}
			continue
		}
		if err := l.paste(ctx, pane, buffer, k); err != nil {
			return err
		}
	}
	return nil
}

// paste writes one text into pane through buffer, and deletes the buffer
// when that fails.
func (l *Lister) paste(ctx context.Context, pane, buffer string, k control.HostKey) error {
	_, err := l.execIn(ctx, []byte(k.Text), loadBufferCmd, buffer, "-")
	if err == nil {
		args := []string{buffer, "-t", pane}
		if k.Paste {
			args = append(args, "-p")
		}
		_, err = l.exec(ctx, pasteBufferCmd, args...)
	}
	if err != nil {
		// A context that ended must not keep the text in tmux.
		if derr := l.DropBuffer(context.WithoutCancel(ctx), buffer); derr != nil {
			return errors.Join(err, derr)
		}
	}
	return err
}

// DropBuffer deletes buffer, one of this instance's; a buffer that is not
// there is no error.
func (l *Lister) DropBuffer(ctx context.Context, buffer string) error {
	if !l.ownBuffer(buffer) {
		return errors.New("hostterm: not a buffer of this instance's host input")
	}
	if _, err := l.exec(ctx, deleteBufferCmd, buffer); err != nil && !noBuffer(err) && !noServer(err) {
		return err
	}
	return nil
}

// DropLeftovers deletes every buffer of this instance's operations that
// tmux still has, such as one an earlier run of the daemon left when it
// stopped between load-buffer and paste-buffer, and returns how many. It
// touches no other buffer. Without a tmux server, or without tmux, there
// are none.
func (l *Lister) DropLeftovers(ctx context.Context) (int, error) {
	if l.Buffers == "" {
		return 0, errors.New("hostterm: no buffer prefix: host input is not set up")
	}
	if l.run == nil {
		if _, err := exec.LookPath("tmux"); err != nil {
			return 0, nil
		}
	}
	out, err := l.exec(ctx, listBuffersCmd)
	if err != nil {
		if noServer(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, name := range strings.Split(string(out), "\n") {
		if !l.ownBuffer(name) {
			continue
		}
		if err := l.DropBuffer(ctx, name); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// noBuffer reports whether a tmux error says that the buffer is not there.
func noBuffer(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unknown buffer") || strings.Contains(s, "no buffer")
}
