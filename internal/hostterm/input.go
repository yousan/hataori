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
	"time"

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

// cmdTimeout bounds each command that types or deletes a buffer.
const cmdTimeout = 10 * time.Second

// whole returns a context for one command of a batch: it is not cut short
// when ctx ends, since killing a tmux client does not undo what it already
// asked of the server (a paste after the end, a buffer made after its
// deletion), but it has a timeout of its own.
func whole(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cmdTimeout)
}

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
// runs. When loading or pasting fails, the buffer is deleted. Once ctx
// ends, no further command of the batch starts; one that has started runs
// to its end. No error holds a key or a text.
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
		if err := ctx.Err(); err != nil { // the operation ended: nothing more of the batch
			return fmt.Errorf("hostterm: the batch was stopped: %w", err)
		}
		if k.Key != "" {
			cctx, cancel := whole(ctx)
			_, err := l.exec(cctx, sendKeysCmd, pane, keyArg(k.Key))
			cancel()
			if err != nil {
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

// keyArg is a key name as tmux's command line takes it. tmux ends a
// command at an argument that ends in ";", and takes "\\;" at the end for a
// ";": unescaped, "M-;" would reach the pane as the text "M-".
func keyArg(name string) string {
	if strings.HasSuffix(name, ";") {
		return name[:len(name)-1] + `\;`
	}
	return name
}

// paste writes one text into pane through buffer, and deletes the buffer
// when that fails, or when ctx ended after the load.
func (l *Lister) paste(ctx context.Context, pane, buffer string, k control.HostKey) error {
	cctx, cancel := whole(ctx)
	_, err := l.execIn(cctx, []byte(k.Text), loadBufferCmd, buffer, "-")
	cancel()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		args := []string{buffer, "-t", pane}
		if k.Paste {
			args = append(args, "-p")
		}
		cctx, cancel := whole(ctx)
		_, err = l.exec(cctx, pasteBufferCmd, args...)
		cancel()
	}
	if err != nil {
		cctx, cancel := whole(ctx)
		derr := l.DropBuffer(cctx, buffer)
		cancel()
		if derr != nil {
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
