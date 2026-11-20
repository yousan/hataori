package claudecode

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
)

// The codec reads the screen from Open until the first input is reported,
// for two things no hook tells.
//
// The folder-trust dialog (design, section 3.5): the first start in a
// repository shows it, and no hook runs until it is answered. The codec
// knows only this dialog, by its text as captured, and answers it by keys
// before the session starts: once the screen has settled, it moves the
// cursor one line at a time toward the line that says yes, found by its
// text, and confirms once the cursor is on it. Any other screen gets no
// key, and the start fails on its timeout.
//
// When the folder holds files that trusting it loads, such as a project
// settings file, the dialog names them on one more line (2.1.328, #423).
// A confined session gets the dialog answered whatever it names: its
// launch ignores those files. An ordinary session gets it answered only
// when Spec.TrustFiles lists every file it names; otherwise no key is
// sent, and the start fails at once as blocked at start, naming them.
//
// The first input (#178): SessionStart is reported before the CLI reads its
// terminal as keys. Input written in between reaches the terminal's line
// discipline: the paste lands in the input box later, but its Enter turns
// into a new line (seen with 2.1.327, under the user's own settings). So
// the first paste waits until the screen shows the CLI switching bracketed
// paste on after the dialog, as its input box does; Enter follows on a
// Tick after pasteSettle; and while the box still shows the text, Enter is
// pressed again.

// trustHeader is the dialog's text above its choices, with the workspace's
// path in place of %s, its lines joined by single spaces.
// The line naming files, if any, comes between %s and "Security guide".
const trustHeader = "Accessing workspace: %s Quick safety check: Is this a project you created or one you trust? " +
	"(Like your own code, a well-known open source project, or work from your team). If not, " +
	"take a moment to review what's in this folder first. " +
	"Claude Code'll be able to read, edit, and execute files here.%s Security guide"

// The line of the dialog that names the files trusting the folder loads,
// as 2.1.328 writes it: "Trusting this folder loads .claude/settings.json.",
// "... loads .claude/settings.json and .claude/settings.local.json (runs
// commands and sets environment variables).", "... loads .mcp.json here or
// above (MCP servers)." (testdata/2.1.328/trust_*).
const (
	loadsPrefix = "Trusting this folder loads "
	loadsAbove  = " here or above"
)

const (
	trustYes    = "Yes, I trust this folder"
	trustNo     = "No, exit"
	trustFooter = "Enter to confirm · Esc to cancel"
	cursorMark  = "❯"
)

// The keys the codec sends: one move toward the line that says yes at a
// time, then Enter once the cursor is on it.
var (
	keyUp    = agent.Part{Key: agent.Up}
	keyDown  = agent.Part{Key: agent.Down}
	keyEnter = agent.Part{Key: agent.Enter}
)

// maxScreen bounds what the codec keeps of the screen; past it, it stops
// looking.
const maxScreen = 256 << 10

// Bracketed paste switched on and off, as the CLI tells the terminal.
var (
	pasteOn  = []byte("\x1b[?2004h")
	pasteOff = []byte("\x1b[?2004l")
)

// pasteWait is how long the first input waits for the screen to show the
// CLI takes pastes. After it the input is pasted all the same, as it was
// before the codec waited.
const pasteWait = 10 * time.Second

// againWait is how long after Enter the input box may still show the text
// before Enter is pressed again; maxAgain bounds how often.
const (
	againWait = 2 * time.Second
	maxAgain  = 2
)

// trustDialog reads the screen as the trust dialog for the workspace dir:
// the line the cursor is on and the line that says yes, counted among the
// choices, and the files the dialog says trusting the folder loads, if it
// names any. ok is false when the screen is anything else.
func trustDialog(screen []string, dir string) (cursor, yes int, files []string, ok bool) {
	lines := trimLines(screen)
	footer := -1
	for i, l := range lines {
		if l == trustFooter {
			footer = i
		}
	}
	if footer < 0 || footer != len(lines)-1 {
		return 0, 0, nil, false // the footer is the dialog's last line
	}
	// The choices: the lines just above the blank line above the footer.
	end := footer - 1
	if end < 0 || lines[end] != "" {
		return 0, 0, nil, false
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
				return 0, 0, nil, false
			}
			cursor = i
			l = rest
		}
		choices = append(choices, strings.TrimSpace(l))
	}
	if cursor < 0 || len(choices) != 2 {
		return 0, 0, nil, false
	}
	for i, c := range choices {
		switch c {
		case trustYes:
			yes = i
		case trustNo:
		default:
			return 0, 0, nil, false
		}
	}
	if yes < 0 || choices[0] == choices[1] {
		return 0, 0, nil, false
	}
	// The text above the choices: a top rule, then the header.
	var words []string
	for _, l := range lines[:start] {
		if l != "" && strings.Trim(l, "─") == "" {
			continue
		}
		words = append(words, strings.Fields(l)...)
	}
	files, ok = headerMatches(strings.Join(words, " "), dir)
	if !ok {
		return 0, 0, nil, false
	}
	return cursor, yes, files, true
}

// headerMatches reports whether text is the dialog's header for the
// workspace dir, and returns the files its line about them names. The path
// may be wrapped, so it is compared without spaces.
func headerMatches(text, dir string) ([]string, bool) {
	parts := strings.Split(trustHeader, "%s")
	if len(parts) != 3 || dir == "" || !strings.HasPrefix(text, parts[0]) {
		return nil, false
	}
	path, rest, ok := strings.Cut(text[len(parts[0]):], parts[1])
	nospace := func(s string) string { return strings.Join(strings.Fields(s), "") }
	if !ok || nospace(path) != nospace(dir) {
		return nil, false
	}
	loads, ok := strings.CutSuffix(rest, parts[2])
	if !ok {
		return nil, false
	}
	if loads == "" {
		return nil, true
	}
	files := loadedFiles(strings.TrimPrefix(loads, " "))
	return files, files != nil
}

// loadedFiles reads the dialog's line about the files trusting the folder
// loads: the files it names, or nil when the line is not of that shape.
func loadedFiles(line string) []string {
	list, ok := strings.CutPrefix(line, loadsPrefix)
	if !ok {
		return nil
	}
	list, ok = strings.CutSuffix(list, ".")
	if !ok {
		return nil
	}
	if i := strings.LastIndex(list, " ("); i >= 0 && strings.HasSuffix(list, ")") {
		list = list[:i] // what the files do, such as "(runs commands)"
	}
	list = strings.TrimSuffix(list, loadsAbove)
	var files []string
	for _, f := range strings.FieldsFunc(strings.ReplaceAll(list, " and ", ","), func(r rune) bool { return r == ',' }) {
		f = strings.TrimSpace(f)
		if f == "" || strings.ContainsAny(f, " ()") {
			return nil
		}
		files = append(files, f)
	}
	return files
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
// paste and theme reports (seen: 25, 1000, 1002, 1003, 1004, 1006, 1016,
// 2004, 2031).
var inertModes = map[string]bool{"12": true, "25": true, "1000": true, "1002": true, "1003": true, "1004": true,
	"1005": true, "1006": true, "1015": true, "1016": true, "2004": true, "2031": true}

// altScreen is the private mode of the alternate screen that saves the
// cursor and clears the screen: xterm's 1049.
const altScreen = "1049"

// inertOSC are the operating system commands that change nothing shown: a
// title (0, 1, 2), a link around text (8), and a query (7501, seen).
var inertOSC = map[string]bool{"0": true, "1": true, "2": true, "8": true, "7501": true}

// inertSGR reports whether a graphic rendition leaves the text readable:
// colours and weights do, concealing it (8) does not.
func inertSGR(params string) bool {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		switch ps[i] {
		case "8":
			return false
		case "38", "48", "58": // an extended colour: its arguments follow
			if i+1 < len(ps) && ps[i+1] == "5" {
				i += 2
			} else if i+1 < len(ps) && ps[i+1] == "2" {
				i += 4
			}
		}
	}
	return true
}

// renderScreen plays terminal output onto a grid of lines and returns them.
// It knows only what the CLI uses to draw: text, tab, carriage return, line
// feed, backspace, saving and restoring the cursor, and the cursor
// movements and erasures of ECMA-48. Colours, cursor shape, terminal
// queries, titles, links and the modes that do not change what is shown
// are skipped. The alternate screen (1049, as the CLI's full-screen
// interface uses it) is a grid of its own. Anything else that could change
// the screen (a reset, scrolling, inserting or deleting lines or
// characters) makes ok false: such a screen is not read. The grid has no height: it
// grows as the cursor goes down, and a cursor position counts from its top.
func renderScreen(b []byte) (screen []string, ok bool) {
	var grid, mainGrid [][]rune
	row, col := 0, 0
	savedRow, savedCol := 0, 0
	alt, mainRow, mainCol := false, 0, 0 // on the alternate screen; the main screen and its cursor
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
						switch {
						case inertModes[m]:
						case m == altScreen && final == 'h' && !alt:
							// The cursor saved, a blank screen of its own.
							alt, mainGrid, mainRow, mainCol = true, grid, row, col
							grid = nil
						case m == altScreen && final == 'l' && alt:
							alt, grid, row, col = false, mainGrid, mainRow, mainCol
						default:
							return nil, false // a clear, or a mode not known
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
			case 'm':
				if !inertSGR(params) {
					return nil, false
				}
			case 'c', 'n':
				// queries of the terminal
			case 'r':
				if params != "" {
					return nil, false // a scrolling region
				}
				row, col = 0, 0 // resetting the margins homes the cursor
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
			// An operating system command, ended by BEL or ST.
			num := i + 2
			for num < len(b) && b[num] >= '0' && b[num] <= '9' {
				num++
			}
			if num < len(b) && !inertOSC[string(b[i+2:num])] {
				return nil, false // an image, a palette change, or one not known
			}
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
				if i+2 >= len(b) {
					return gridLines(grid), true
				}
				if b[i+2] != 'B' {
					return nil, false // a character set other than ASCII
				}
				i += 3
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

// screenState is what the codec keeps of the screen while it watches.
type screenState struct {
	seen    []byte    // the terminal's output so far
	changed bool      // output came since the last tick
	still   time.Time // the tick that last saw the screen change
	quiet   time.Time // since when the screen has not changed; zero until it changed after the last key
	done    bool      // the dialog was confirmed, or the codec stopped looking for it
	moved   int       // moves sent
	from    int       // the cursor line the last move was sent from
	mark    int       // how much of seen came before the dialog was confirmed
	held    time.Time // the first tick that held the first input back
	anyway  bool      // pasteWait passed: the first input goes without the screen's word
}

// screen keeps more of the terminal's output, and writes the first input
// if the screen now shows the CLI takes it. Keys for the dialog are sent by
// Tick, once the screen has settled.
func (c *codec) screen(b []byte) Actions {
	s := &c.scr
	if !c.watch {
		return Actions{}
	}
	s.seen = append(s.seen, b...)
	s.changed = true
	if len(s.seen) > maxScreen {
		return c.endWatch(true)
	}
	return c.flush()
}

// endWatch ends the watch of the screen once the first input is reported
// or has failed, or the session ended; with force, now. Input held for the
// screen is written then.
func (c *codec) endWatch(force bool) Actions {
	if !c.watch || !(force || c.closed || (c.wrote && c.sent == nil)) {
		return Actions{}
	}
	c.watch, c.scr = false, screenState{}
	return merge(Actions{StopScreen: true}, c.flush())
}

// pasteReady reports whether input may be pasted, as far as the screen
// goes: when the screen is not watched, when it shows bracketed paste
// switched on since the dialog was confirmed and not off since, or after
// pasteWait.
func (c *codec) pasteReady() bool {
	s := &c.scr
	if !c.watch || s.anyway {
		return true
	}
	on := bytes.LastIndex(s.seen, pasteOn)
	return on >= s.mark && on > bytes.LastIndex(s.seen, pasteOff)
}

// holdPaste counts pasteWait for input that only the screen holds back,
// and writes it once that has passed.
func (c *codec) holdPaste(now time.Time) Actions {
	s := &c.scr
	if !c.watch || !c.ready || c.running || len(c.queue) == 0 || c.pasteReady() {
		return Actions{}
	}
	if s.held.IsZero() {
		s.held = now
		return Actions{}
	}
	if now.Sub(s.held) < pasteWait {
		return Actions{}
	}
	s.anyway = true
	return c.flush()
}

// stillInBox reports whether Enter should be pressed again for the input
// just sent: the screen is watched, has not changed for screenSettle, Enter
// was pressed at least againWait ago and fewer than maxAgain times again,
// and the input box still holds the text.
func (c *codec) stillInBox(now time.Time) bool {
	s := &c.scr
	if !c.watch || c.sent.again >= maxAgain || now.Sub(c.sent.entered) < againWait ||
		s.changed || s.still.IsZero() || now.Sub(s.still) < screenSettle {
		return false
	}
	screen, ok := renderScreen(s.seen)
	return ok && boxHolds(screen, c.sent.typed)
}

// boxHolds reports whether the screen's input box, the last line that
// starts with the prompt mark and the lines below it up to a rule, with a
// rule above it, shows text: the same words, or the CLI's placeholder for
// a long paste.
func boxHolds(screen []string, text string) bool {
	rule := func(l string) bool {
		l = strings.TrimSpace(l)
		return l != "" && strings.Trim(l, "─") == ""
	}
	for i := len(screen) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(strings.TrimSpace(screen[i]), cursorMark)
		if !ok {
			continue
		}
		if i == 0 || !rule(screen[i-1]) {
			return false
		}
		words := strings.Fields(rest)
		j := i + 1
		for ; j < len(screen) && !rule(screen[j]); j++ {
			words = append(words, strings.Fields(screen[j])...)
		}
		if j == len(screen) {
			return false // no rule below: not the box
		}
		got := strings.Join(words, " ")
		return got != "" && (got == strings.Join(strings.Fields(text), " ") || strings.HasPrefix(got, "[Pasted text #"))
	}
	return false
}

// tickScreen notes when the screen last changed, and returns the key the
// trust dialog needs now, if the screen shows it and has not changed for
// screenSettle. A move is sent only once the screen shows where the last
// one took the cursor.
func (c *codec) tickScreen(now time.Time) Actions {
	s := &c.scr
	if !c.watch || len(s.seen) == 0 {
		return Actions{}
	}
	if s.changed {
		s.changed, s.quiet, s.still = false, now, now
		return Actions{}
	}
	if c.started || s.done {
		return Actions{}
	}
	if s.quiet.IsZero() || now.Sub(s.quiet) < screenSettle {
		return Actions{}
	}
	screen, ok := renderScreen(s.seen)
	if !ok {
		s.done = true // a screen it cannot read: no key, ever
		return Actions{}
	}
	cursor, yes, files, ok := trustDialog(screen, c.spec.Dir)
	if !ok || (s.moved > 0 && cursor == s.from) {
		return Actions{}
	}
	if refused := c.untrusted(files); len(refused) > 0 {
		// Not answered: the start fails now, naming what the dialog loads.
		s.done = true
		return c.events(agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart,
			Text: blockedText(screen, refused, c.spec.Scope)})
	}
	s.quiet = time.Time{} // the next key waits for the screen to change and settle again
	switch {
	case cursor == yes:
		s.done, s.mark = true, len(s.seen)
		return Actions{Keys: []agent.Part{keyEnter}}
	case s.moved >= 2: // two choices: never more than one move is needed
		s.done = true
		return Actions{}
	case cursor < yes:
		s.moved, s.from = s.moved+1, cursor
		return Actions{Keys: []agent.Part{keyDown}}
	default:
		s.moved, s.from = s.moved+1, cursor
		return Actions{Keys: []agent.Part{keyUp}}
	}
}

// blockedText is what a person reads of a start blocked at the trust
// dialog (#589): why hataori stopped, in plain words; the dialog as the
// CLI drew it, below its header, one quoted line each; and what to do
// next: list the files refused under the scope's trust_files
// (docs/configuration.md).
func blockedText(screen []string, refused []string, scope string) string {
	var b strings.Builder
	b.WriteString("hataori did not answer Claude Code's folder-trust dialog for you, because the dialog names files of the repository: " +
		"trusting the folder lets the repository's own settings run commands without asking. " +
		"hataori stops for any such file, whatever it holds; it has not judged this repository dangerous.\n")
	if lines := dialogBody(screen); len(lines) > 0 {
		b.WriteString("Claude Code showed:\n")
		for _, l := range lines {
			b.WriteString("> " + l + "\n")
		}
	}
	b.WriteString("What to do next: to let " + strings.Join(refused, ", ") + " through for this scope's threads, list ")
	if len(refused) == 1 {
		b.WriteString("it")
	} else {
		b.WriteString("them")
	}
	b.WriteString(" under \"trust_files\" of the scope \"" + scope + "\" in hataori's config, and start again.")
	return b.String()
}

// dialogBody is the trust dialog's text below its header ("... execute
// files here."): the lines naming the files, the guide's link and the
// choices, as the screen shows them, without the blank lines.
func dialogBody(screen []string) []string {
	lines := trimLines(screen)
	for i, l := range lines {
		if !strings.HasSuffix(l, "execute files here.") {
			continue
		}
		var out []string
		for _, l := range lines[i+1:] {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	return nil
}

// untrusted returns the files of the trust dialog's list that the codec may
// not answer yes for: none in a confined session, whose launch ignores the
// project's files (design, section 7.1), and those Spec.TrustFiles does not
// list in an ordinary one.
func (c *codec) untrusted(files []string) []string {
	if c.spec.Confine != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		if !slices.Contains(c.spec.TrustFiles, f) {
			out = append(out, f)
		}
	}
	return out
}
