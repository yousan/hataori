package supervisor

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/forge"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
)

// The layers of a supervisor's instructions (design, section 7.1, "Two
// layers"): every launch of its sessions is given hataori's layer, then the
// config's, then the user's files, in that order; an earlier layer wins
// over a later one. Nothing in them grants anything: what the supervisor
// may do is its token's Caller, which none of them reaches.

// UserFiles are the user's layer, in the order a launch gives them: plain
// files at the top level of the scope's directory, each optional.
var UserFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "IDENTITY.md"}

// UserFileMax is the largest user's file a launch gives; a larger one is
// skipped.
const UserFileMax = 32 << 10

// UserFile is one of the user's files as it is now in a scope's directory.
type UserFile struct {
	Name    string
	Text    string // its content, when Given
	Given   bool   // a regular file within UserFileMax, read
	Skipped string // why it is not given, when it is there but not Given; empty when it is not there
}

// Hash is what tells one content of f from another: "" when it is not
// given, whether it is not there or skipped.
func (f UserFile) Hash() string {
	if !f.Given {
		return ""
	}
	sum := sha256.Sum256([]byte(f.Text))
	return hex.EncodeToString(sum[:])
}

// ReadUserFiles reads the user's files at the top level of dir, in the
// order of UserFiles. Each is read as text, and nothing in it is followed:
// no import and no link. A symbolic link, a file that is not regular, and
// one larger than UserFileMax are skipped, each with its reason; so is
// every file when dir cannot be opened.
func ReadUserFiles(dir string) []UserFile {
	files := make([]UserFile, len(UserFiles))
	root, err := os.OpenRoot(dir)
	for i, name := range UserFiles {
		files[i] = UserFile{Name: name}
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			files[i].Skipped = "the directory cannot be read: " + err.Error()
		default:
			files[i].Text, files[i].Given, files[i].Skipped = readUserFile(root, name)
		}
	}
	if root != nil {
		root.Close()
	}
	return files
}

func readUserFile(root *os.Root, name string) (string, bool, string) {
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, ""
	case err != nil:
		return "", false, err.Error()
	}
	if why := notGiven(fi); why != "" {
		return "", false, why
	}
	// Not waiting on a pipe, should the file be replaced by one since it
	// was looked at. os.Root follows a link that stays in the directory
	// whatever the flags, so what was opened must be what was looked at.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, err.Error()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", false, err.Error()
	}
	if !os.SameFile(fi, opened) {
		return "", false, "replaced while it was read"
	}
	if why := notGiven(opened); why != "" {
		return "", false, why
	}
	b, err := io.ReadAll(io.LimitReader(f, UserFileMax+1))
	if err != nil {
		return "", false, err.Error()
	}
	if len(b) > UserFileMax {
		return "", false, "larger than 32 KB"
	}
	return string(b), true, ""
}

// notGiven is why a file of fi is not given; empty when it is.
func notGiven(fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "a symbolic link"
	case !fi.Mode().IsRegular():
		return "not a regular file"
	case fi.Size() > UserFileMax:
		return "larger than 32 KB"
	}
	return ""
}

// ConfigLayer is the config's layer of sv: the keys of
// scopes[].supervisor that the daemon acts on, as the loaded config sets
// them, then the config's instructions. forges are those of the instance,
// by which it says what of repos the forge tools are refused for (#795).
func ConfigLayer(sv Scope, forges map[string]forge.Forge) string {
	var b strings.Builder
	b.WriteString("# The config's layer\n\nThe operator's settings for this channel, from hataori's config. The daemon enforces them; only the operator changes them.\n\n")
	autonomy := sv.Autonomy
	if autonomy == "" {
		autonomy = Propose
	}
	fmt.Fprintf(&b, "- autonomy: %s. ", autonomy)
	switch autonomy {
	case Watch:
		b.WriteString("You read threads and tell people in your thread; you do not send, start or ask to interrupt.\n")
	case Act:
		b.WriteString("Your sends and starts run at once.\n")
	default:
		b.WriteString("Your sends and starts are proposals that a person of this channel approves before they run.\n")
	}
	if len(sv.Repos) == 0 {
		b.WriteString("- repos: none. You start no thread, and read no forge.\n")
	} else {
		var names, served, unserved []string
		for _, r := range sv.Repos {
			name := cmp.Or(r.Forge, repository(r.Address))
			names = append(names, name)
			if _, _, ok := forgeOf(forges, r); ok {
				served = append(served, name)
			} else {
				unserved = append(unserved, name)
			}
		}
		fmt.Fprintf(&b, "- repos: %s. The only repositories you may start a thread in, and read on their forge.", strings.Join(names, ", "))
		if len(unserved) > 0 { // Reads refuses these, whatever is said here
			if len(served) > 0 {
				fmt.Fprintf(&b, " A forge answers for %s.", strings.Join(served, ", "))
			}
			fmt.Fprintf(&b, " No forge answers for %s: there the forge tools, `%s` included, are refused, so do not call them. "+
				"Say what a thread claims, and that you did not check it", strings.Join(unserved, ", "), ToolDeployed)
			if autonomy != Watch {
				b.WriteString("; when a person needs the check, start a `research` thread for it")
			}
			b.WriteString(".")
		}
		b.WriteString("\n")
	}
	// What Budget's API refuses and its Admit holds.
	fmt.Fprintf(&b, "- Budget: turns_per_hour %s, sends_per_hour %s (a start counts as a send, and so does setting a reminder), tokens_per_day %s. "+
		"Once sends_per_hour is used up, your sends, starts and reminders are refused until the hour ends; the other two limits refuse none of them. "+
		"Once any limit is used up, what would wake you waits until the hour or the day ends, with two exceptions: "+
		"the first turn end of a thread you started for a person's message, and a thread you started that fails to start "+
		"(one that no person asked for only up to twice turns_per_hour). tokens_per_day holds these two as well.\n",
		limit(int64(sv.Limits.Turns)), limit(int64(sv.Limits.Sends)), limit(sv.Limits.Tokens))
	if sv.RemindersMax > 0 {
		fmt.Fprintf(&b, "- reminders_max: %d. The most reminders you may have open; setting one counts as a send.\n", sv.RemindersMax)
	} else {
		b.WriteString("- reminders_max: 0. You set no reminder.\n")
	}
	switch sv.Report {
	case instance.ReportNeeded:
		b.WriteString("- report: needed. A wake's final answer is posted only when it names a person with @<name>; the others stay in your thread's events.\n")
	case instance.ReportDigest:
		fmt.Fprintf(&b, "- report: digest, report_every %s. A wake's final answer that names a person with @<name> is posted at once; the others are posted together, once every %s.\n",
			duration(sv.ReportEvery), duration(sv.ReportEvery))
	default:
		b.WriteString("- report: every. Each wake's final answer is posted, unless it is exactly (quiet).\n")
	}
	if sv.Mention == instance.MentionNever {
		b.WriteString("- mention: never. Nothing you post mentions anyone; @<name> stays text.\n")
	} else {
		b.WriteString("- mention: named. @<name> of a person of this channel mentions them; nothing else you post mentions anyone.\n")
	}
	if q := sv.QuietHours; q != nil {
		zone := "the daemon's time zone"
		if q.Zone != nil && q.Zone != time.Local {
			zone = q.Zone.String()
		}
		fmt.Fprintf(&b, "- quiet_hours: from %02d:%02d to %02d:%02d, %s. During them nothing you post mentions anyone; when they end, one message mentions each person named during them.\n",
			q.From/60, q.From%60, q.To/60, q.To%60, zone)
	}
	if sv.Instructions != "" {
		b.WriteString("\n## The config's instructions\n\n")
		b.WriteString(sv.Instructions)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// duration is d as a config writes it, such as 1h or 90m.
func duration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func limit(n int64) string {
	if n == 0 {
		return "no limit"
	}
	return strconv.FormatInt(n, 10)
}

// Compose is the instructions of one launch of sv's session, in an
// instance with forges: hataori's layer, the config's layer, then each of the user's files that is given,
// under a heading with its name, in the order of files.
func Compose(sv Scope, forges map[string]forge.Forge, files []UserFile) string {
	parts := []string{Layer, ConfigLayer(sv, forges)}
	var user []string
	for _, f := range files {
		if f.Given {
			user = append(user, "## "+f.Name+"\n\n"+strings.TrimRight(f.Text, "\n"))
		}
	}
	if len(user) > 0 {
		parts = append(parts, "# The user's files\n\nThe user's layer, from your directory. Each is text, given as it is.\n\n"+strings.Join(user, "\n\n"))
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// given records only the user files actually supplied by this launch.
func (s *Sessions) given(ctx context.Context, sid int64, dir string, files []UserFile) error {
	root, _ := os.OpenRoot(dir)
	if root != nil {
		defer root.Close()
	}
	return s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, f := range files {
			hash := f.Hash()
			if !f.Given && root != nil {
				if st, err := root.Lstat(f.Name); err == nil && st.Mode().IsRegular() {
					hash = fmt.Sprintf("stat:%d:%d", st.Size(), st.ModTime().UnixNano())
				}
			}
			if err := remember(ctx, tx, sid, f.Name, hash); err != nil {
				return err
			}
		}
		return nil
	})
}

// recorded is the own thread's current session's record, for callers that
// need the legacy scope view. Conversations have separate rows.
func (s *Sessions) recorded(ctx context.Context, scope string) (map[string]string, error) {
	var sid int64
	err := s.Core.Store.DB.QueryRowContext(ctx, `SELECT s.id FROM sessions s JOIN threads t ON t.id = s.thread_id
 WHERE t.scope = ? AND t.kind = 'supervisor' ORDER BY t.id DESC, s.id DESC LIMIT 1`, scope).Scan(&sid)
	if err != nil {
		return nil, err
	}
	return recordedSession(ctx, s.Core.Store.DB, sid)
}

// Change is the last told and current fingerprint of a file named in a wake.
// Session keeps an accepted wake from advancing a replacement session.
type Change struct {
	Name    string `json:"name"`
	Was     string `json:"was"`
	Now     string `json:"now"`
	Session int64  `json:"session,omitempty"`
}

func (s *Sessions) Changed(ctx context.Context, scope string, thread control.ThreadID) ([]Change, error) {
	sid, err := sessionID(ctx, s.Core.Store.DB, thread)
	if err != nil {
		return nil, err
	}
	var dir string
	if err := s.Core.Store.DB.QueryRowContext(ctx, `SELECT workspace FROM threads WHERE id = ?`, int64(thread)).Scan(&dir); err != nil {
		return nil, err
	}
	now, err := s.memory(ctx, dir)
	if err != nil {
		return nil, err
	}
	told, err := recordedSession(ctx, s.Core.Store.DB, sid)
	if err != nil {
		return nil, err
	}
	return memoryChanges(sid, told, now), nil
}

// Told advances the fingerprints named by an accepted wake, unless a
// launch or file call supplied newer content in the meantime.
func (s *Sessions) Told(ctx context.Context, scope string, changes []Change) error {
	return s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, c := range changes {
			if c.Session == 0 {
				// A wake persisted by an older daemon belongs to the own thread.
				if err := tx.QueryRowContext(ctx, `SELECT s.id FROM sessions s JOIN threads t ON t.id = s.thread_id
     WHERE t.scope = ? AND t.kind = 'supervisor' ORDER BY t.id DESC, s.id DESC LIMIT 1`, scope).Scan(&c.Session); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor_session_files (session_id, name, told) VALUES (?, ?, ?)
    ON CONFLICT (session_id, name) DO UPDATE SET told = CASE WHEN told = ? THEN excluded.told ELSE told END`, c.Session, c.Name, c.Now, c.Was); err != nil {
				return err
			}
		}
		return nil
	})
}

// Ended is part of Files, as ScopeFiles.Ended.
func (s *Sessions) Ended(ctx context.Context, scope string, thread control.ThreadID, pos uint64) error {
	if s.Files == nil {
		return nil
	}
	return s.Files.Ended(ctx, scope, thread, pos)
}

// Untold is part of Files, as ScopeFiles.Untold.
func (s *Sessions) Untold(ctx context.Context, scope string) ([]FilesTurn, error) {
	if s.Files == nil {
		return nil, nil
	}
	return s.Files.Untold(ctx, scope)
}

// Named is part of Files, as ScopeFiles.Named.
func (s *Sessions) Named(ctx context.Context, scope string, t FilesTurn) error {
	if s.Files == nil {
		return nil
	}
	return s.Files.Named(ctx, scope, t)
}

// Files is what a Waker asks about the user's files of its scope (design,
// section 7.1, "Two layers"): before each wake, which changed since the
// session was last given or told them, to name them in the wake; and after
// each turn of a supervisor's thread, which of them it wrote, to name them
// in a line. Sessions is one.
type Files interface {
	Changed(ctx context.Context, scope string, thread control.ThreadID) ([]Change, error)
	Told(ctx context.Context, scope string, changes []Change) error

	Ended(ctx context.Context, scope string, thread control.ThreadID, pos uint64) error
	Untold(ctx context.Context, scope string) ([]FilesTurn, error)
	Named(ctx context.Context, scope string, t FilesTurn) error
}

var _ Files = (*Sessions)(nil)

// FilesKeyPrefix starts the key of the line that names the user's files a
// supervisor's turn changed; the rest is the position of the event that
// ended it and those names. The line about a conversation's turn in the
// supervisor's own thread has filesOwnKey after it.
const (
	FilesKeyPrefix = "hataori-files-"
	filesOwnKey    = "-own"
)

// FilesNotice is the line that names the user's files a turn of the
// supervisor changed, in the thread of that turn.
func FilesNotice(names []string) string {
	return "Changed by this turn, in the supervisor's instructions: " + list(names) + "."
}

// FilesNoticeOf is that line in the supervisor's own thread, about a turn
// of the conversation thread.
func FilesNoticeOf(thread control.ThreadID, names []string) string {
	return fmt.Sprintf("Changed by a turn of thread #%d, in the supervisor's instructions: %s.", int64(thread), list(names))
}

// FilesChanged is the part of a wake that names the user's files that
// changed since the session was last given or told them.
func FilesChanged(names []string) string {
	for _, name := range names {
		if strings.HasPrefix(name, "notes/") {
			return "Changed in your directory since you were given them: " + list(names) + ". Read them again."
		}
	}
	return "Changed in your directory since you were given them: " + list(names) + ". Read them again: they are the user's layer of your instructions."
}

func list(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
