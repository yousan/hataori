package claudecode

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The folder-trust dialog (design, section 3.5) is the one screen the codec
// reads. The first start in a repository shows it, and no hook runs until it
// is answered. The codec knows only this dialog, by its text as captured,
// and answers it by keys before the session starts: once the screen has
// settled, it moves the cursor one line at a time toward the line that says
// yes, found by its text, and confirms once the cursor is on it. Any other screen gets no key, and the
// start fails on its timeout.

// trustHeader is the dialog's text above its choices, with the workspace's
// path in place of %s, its lines joined by single spaces.
const trustHeader = "Accessing workspace: %s Quick safety check: Is this a project you created or one you trust? " +
	"(Like your own code, a well-known open source project, or work from your team). If not, " +
	"take a moment to review what's in this folder first. " +
	"Claude Code'll be able to read, edit, and execute files here. Security guide"

const (
	trustYes    = "Yes, I trust this folder"
	trustNo     = "No, exit"
	trustFooter = "Enter to confirm · Esc to cancel"
	cursorMark  = "❯"
)

// The keys the codec sends: one move toward the line that says yes at a
// time, then Enter once the cursor is on it.
var (
	keyUp    = []byte("\x1b[A")
	keyDown  = []byte("\x1b[B")
	keyEnter = []byte("\r")
)

// maxScreen bounds what the codec keeps of the screen before the start;
// past it, it stops looking.
const maxScreen = 256 << 10

// trustDialog reads the screen as the trust dialog for the workspace dir:
// the line the cursor is on and the line that says yes, counted among the
// choices. ok is false when the screen is anything else.
func trustDialog(screen []string, dir string) (cursor, yes int, ok bool) {
	lines := trimLines(screen)
	footer := -1
	for i, l := range lines {
		if l == trustFooter {
			footer = i
		}
	}
	if footer < 0 || footer != len(lines)-1 {
		return 0, 0, false // the footer is the dialog's last line
	}
	// The choices: the lines just above the blank line above the footer.
	end := footer - 1
	if end < 0 || lines[end] != "" {
		return 0, 0, false
	}
	start := end
	for start > 0 && lines[start-1] != "" {
		start--
	}
	var choices []string
	cursor, yes = -1, -1
	for i, l := range lines[start:end] {
		if rest, found := strings.CutPrefix(l, cursorMark); found {
			if cursor >= 0 {
				return 0, 0, false
			}
			cursor = i
			l = rest
		}
		choices = append(choices, strings.TrimSpace(l))
	}
	if cursor < 0 || len(choices) != 2 {
		return 0, 0, false
	}
	for i, c := range choices {
		switch c {
		case trustYes:
			yes = i
		case trustNo:
		default:
			return 0, 0, false
		}
	}
	if yes < 0 || choices[0] == choices[1] {
		return 0, 0, false
	}
	// The text above the choices: a top rule, then the header.
	var words []string
	for _, l := range lines[:start] {
		if l != "" && strings.Trim(l, "─") == "" {
			continue
		}
		words = append(words, strings.Fields(l)...)
	}
	if !headerMatches(strings.Join(words, " "), dir) {
		return 0, 0, false
	}
	return cursor, yes, true
}

// headerMatches reports whether text is the dialog's header for the
// workspace dir. The path may be wrapped, so it is compared without spaces.
func headerMatches(text, dir string) bool {
	before, after, ok := strings.Cut(trustHeader, "%s")
	if !ok || !strings.HasPrefix(text, before) || !strings.HasSuffix(text, after) || len(text) < len(before)+len(after) {
		return false
	}
	path := text[len(before) : len(text)-len(after)]
	nospace := func(s string) string { return strings.Join(strings.Fields(s), "") }
	return dir != "" && nospace(path) == nospace(dir)
}

// trimLines trims the spaces around each line and drops the blank lines
// at the start and the end.
func trimLines(screen []string) []string {
	var out []string
	for _, l := range screen {
		out = append(out, strings.TrimSpace(l))
	}
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// inertModes are the private modes the CLI sets that change nothing shown:
// the cursor's visibility and blinking, mouse and focus reports, bracketed
// paste, synchronized output, and theme reports (seen: 25, 1000, 1002,
// 1003, 1004, 1006, 1016, 2004, 2031).
var inertModes = map[string]bool{"12": true, "25": true, "1000": true, "1002": true, "1003": true, "1004": true,
	"1005": true, "1006": true, "1015": true, "1016": true, "2004": true, "2026": true, "2031": true}

// renderScreen plays terminal output onto a grid of lines and returns them.
// It knows only what the CLI uses to draw: text, tab, carriage return, line
// feed, backspace, saving and restoring the cursor, and the cursor
// movements and erasures of ECMA-48. Colours, cursor shape, terminal
// queries, titles and the modes that do not change what is shown are
// skipped. Anything else that could change the screen (a reset, the
// alternate screen, scrolling, inserting or deleting lines or characters)
// makes ok false: such a screen is not read. The grid has no height: it
// grows as the cursor goes down, and a cursor position counts from its top.
func renderScreen(b []byte) (screen []string, ok bool) {
	var grid [][]rune
	row, col := 0, 0
	savedRow, savedCol := 0, 0
	put := func(r rune) {
		for len(grid) <= row {
			grid = append(grid, nil)
		}
		for len(grid[row]) <= col {
			grid[row] = append(grid[row], ' ')
		}
		grid[row][col] = r
		col++
	}
	line := func() []rune {
		for len(grid) <= row {
			grid = append(grid, nil)
		}
		return grid[row]
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b) && b[i+1] == '[':
			j := i + 2
			for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
				j++
			}
			params := string(b[i+2 : j])
			k := j
			for k < len(b) && b[k] >= 0x20 && b[k] <= 0x2f {
				k++
			}
			inter := string(b[j:k])
			if k >= len(b) {
				return gridLines(grid), true // the rest of the sequence is still to come
			}
			final := b[k]
			i = k + 1
			if inter != "" {
				if final == 'q' {
					continue // cursor shape
				}
				return nil, false
			}
			if p, private := strings.CutPrefix(params, "?"); private {
				switch final {
				case 'h', 'l':
					for _, m := range strings.Split(p, ";") {
						if !inertModes[m] {
							return nil, false // the alternate screen, a clear, or a mode not known
						}
					}
					continue
				case 'u':
					continue // a query of the keyboard protocol
				}
				return nil, false
			}
			if strings.ContainsAny(params, "<=>") {
				if final == 'q' || final == 'c' || final == 'm' || final == 'u' {
					continue // queries of the terminal and keyboard modes
				}
				return nil, false
			}
			n := func(k, def int) int {
				ps := strings.Split(params, ";")
				if k < len(ps) {
					if v, err := strconv.Atoi(ps[k]); err == nil && v > 0 {
						return v
					}
				}
				return def
			}
			switch final {
			case 'm', 'c', 'n', 't':
				// colours, and queries of the terminal
			case 'r':
				if params != "" {
					return nil, false // a scrolling region
				}
			case 's':
				savedRow, savedCol = row, col
			case 'u':
				row, col = savedRow, savedCol
			case 'A':
				row = max(0, row-n(0, 1))
			case 'B':
				row += n(0, 1)
			case 'C':
				col += n(0, 1)
			case 'D':
				col = max(0, col-n(0, 1))
			case 'G':
				col = n(0, 1) - 1
			case 'H', 'f':
				row, col = n(0, 1)-1, n(1, 1)-1
			case 'K':
				l := line()
				switch params {
				case "", "0":
					if col < len(l) {
						grid[row] = l[:col]
					}
				case "1":
					for k := 0; k <= col && k < len(l); k++ {
						l[k] = ' '
					}
				case "2":
					grid[row] = nil
				default:
					return nil, false
				}
			case 'J':
				switch params {
				case "", "0":
					l := line()
					if col < len(l) {
						grid[row] = l[:col]
					}
					grid = grid[:row+1]
				case "2", "3":
					grid = nil
				default:
					return nil, false
				}
			default:
				return nil, false
			}
		case c == 0x1b && i+1 < len(b) && b[i+1] == ']':
			// An operating system command (a title, a query), ended by BEL or ST.
			j := i + 2
			for j < len(b) && b[j] != 0x07 && !(b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
				j++
			}
			if j >= len(b) {
				return gridLines(grid), true
			}
			if b[j] == 0x07 {
				i = j + 1
			} else {
				i = j + 2
			}
		case c == 0x1b:
			if i+1 >= len(b) {
				return gridLines(grid), true
			}
			switch b[i+1] {
			case '7':
				savedRow, savedCol = row, col
				i += 2
			case '8':
				row, col = savedRow, savedCol
				i += 2
			case '=', '>':
				i += 2 // keypad modes
			case '(', ')':
				i += 3 // a character set
			default:
				return nil, false // a reset, an index, or anything else
			}
		case c == '\r':
			col = 0
			i++
		case c == '\n':
			row++
			i++
		case c == '\b':
			col = max(0, col-1)
			i++
		case c == '\t':
			col = (col/8 + 1) * 8
			i++
		case c == 0x07:
			i++ // the bell
		case c < 0x20 || c == 0x7f:
			return nil, false
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				if !utf8.FullRune(b[i:]) {
					return gridLines(grid), true
				}
				return nil, false
			}
			put(r)
			i += size
		}
	}
	return gridLines(grid), true
}

func gridLines(grid [][]rune) []string {
	out := make([]string, len(grid))
	for i, l := range grid {
		out[i] = string(l)
	}
	return out
}

// screenSettle is how long the screen must stay as it is before a key is
// sent. Right after drawing the dialog the CLI asks the terminal what it
// supports and waits for answers that a bare terminal never gives; a key
// sent meanwhile was taken late, twice, and the Enter after it was lost
// (seen with 2.1.326).
const screenSettle = 2 * time.Second

// screenState is what the codec keeps of the screen before the start.
type screenState struct {
	seen    []byte    // the terminal's output so far
	changed bool      // output came since the last tick
	quiet   time.Time // since when the screen has not changed; zero until it changed after the last key
	done    bool      // the dialog was confirmed, or the codec stopped looking
	moved   int       // moves sent
	from    int       // the cursor line the last move was sent from
}

// screen keeps more of the terminal's output. Keys are sent by Tick, once
// the screen has settled.
func (c *codec) screen(b []byte) Actions {
	s := &c.scr
	if c.started || s.done {
		return Actions{}
	}
	s.seen = append(s.seen, b...)
	s.changed = true
	if len(s.seen) > maxScreen {
		s.done, s.seen = true, nil
		return Actions{StopScreen: true}
	}
	return Actions{}
}

// tickScreen returns the key the trust dialog needs now, if the screen
// shows it and has not changed for screenSettle. A move is sent only once
// the screen shows where the last one took the cursor.
func (c *codec) tickScreen(now time.Time) Actions {
	s := &c.scr
	if c.started || s.done || len(s.seen) == 0 {
		return Actions{}
	}
	if s.changed {
		s.changed, s.quiet = false, now
		return Actions{}
	}
	if s.quiet.IsZero() || now.Sub(s.quiet) < screenSettle {
		return Actions{}
	}
	screen, ok := renderScreen(s.seen)
	if !ok {
		s.done, s.seen = true, nil // a screen it cannot read: no key, ever
		return Actions{StopScreen: true}
	}
	cursor, yes, ok := trustDialog(screen, c.spec.Dir)
	if !ok || (s.moved > 0 && cursor == s.from) {
		return Actions{}
	}
	s.quiet = time.Time{} // the next key waits for the screen to change and settle again
	switch {
	case cursor == yes:
		s.done, s.seen = true, nil
		return Actions{Write: [][]byte{keyEnter}, StopScreen: true}
	case s.moved >= 2: // two choices: never more than one move is needed
		s.done, s.seen = true, nil
		return Actions{StopScreen: true}
	case cursor < yes:
		s.moved, s.from = s.moved+1, cursor
		return Actions{Write: [][]byte{keyDown}}
	default:
		s.moved, s.from = s.moved+1, cursor
		return Actions{Write: [][]byte{keyUp}}
	}
}
