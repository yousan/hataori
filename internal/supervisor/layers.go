package supervisor

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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

// hashes are the user's files' Hash, by name.
func hashes(files []UserFile) map[string]string {
	m := make(map[string]string, len(files))
	for _, f := range files {
		m[f.Name] = f.Hash()
	}
	return m
}

// The user's files of a scope as its supervisor sessions were given them
// (told) and as the daemon last looked at them after a supervisor's turn
// (seen), in supervisor_files: one row per scope and file, "" for a file
// not given.

// given records files as given to a launch of scope's session: told, and,
// for a file of the scope seen for the first time, seen too, so that no
// turn is said to have changed what was there before it.
func (s *Sessions) given(ctx context.Context, scope string, files []UserFile) error {
	return s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor_files (scope, name, told, seen) VALUES (?1, ?2, ?3, ?3)
				ON CONFLICT (scope, name) DO UPDATE SET told = excluded.told`, scope, f.Name, f.Hash()); err != nil {
				return fmt.Errorf("supervisor: the user's files given: %w", err)
			}
		}
		return nil
	})
}

// recorded returns the column col (told or seen) of scope's files, by
// name; a file without a row is missing.
func (s *Sessions) recorded(ctx context.Context, scope, col string) (map[string]string, error) {
	rows, err := s.Core.Store.DB.QueryContext(ctx, `SELECT name, `+col+` FROM supervisor_files WHERE scope = ?`, scope)
	if err != nil {
		return nil, fmt.Errorf("supervisor: the user's files: %w", err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var name, h string
		if err := rows.Scan(&name, &h); err != nil {
			return nil, fmt.Errorf("supervisor: the user's files: %w", err)
		}
		m[name] = h
	}
	return m, rows.Err()
}

// now returns the hashes of the user's files of thread's directory as
// they are now: the directory its launches read.
func (s *Sessions) now(ctx context.Context, thread control.ThreadID) (map[string]string, error) {
	var dir string
	if err := s.Core.Store.DB.QueryRowContext(ctx, `SELECT workspace FROM threads WHERE id = ?`, int64(thread)).Scan(&dir); err != nil {
		return nil, fmt.Errorf("supervisor: the directory of thread %d: %w", thread, err)
	}
	return hashes(ReadUserFiles(dir)), nil
}

// differ returns the names of the files whose hash in now is not the one
// in was, a file with no row in was taken as not given, in the order of
// UserFiles.
func differ(was, now map[string]string) []string {
	var names []string
	for _, name := range UserFiles {
		if was[name] != now[name] {
			names = append(names, name)
		}
	}
	return names
}

// Change is one of the user's files that a wake names: its hash as the
// session was last given or told it when the wake was made (Was), and as
// it was then (Now).
type Change struct {
	Name string `json:"name"`
	Was  string `json:"was"`
	Now  string `json:"now"`
}

// Changed is part of Files: the user's files in the directory of thread,
// scope's supervisor thread, whose content differs from what its session
// was last given or told.
func (s *Sessions) Changed(ctx context.Context, scope string, thread control.ThreadID) ([]Change, error) {
	now, err := s.now(ctx, thread)
	if err != nil {
		return nil, err
	}
	told, err := s.recorded(ctx, scope, "told")
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, name := range UserFiles {
		if told[name] != now[name] {
			changes = append(changes, Change{Name: name, Was: told[name], Now: now[name]})
		}
	}
	return changes, nil
}

// Told is part of Files: a wake that named changes was accepted. Each file
// is told as it was when the wake was made, unless a launch gave the
// session another content since (told is no longer Was), which stays;
// either is what the turn the wake starts begins from, so it is seen too,
// and a change made before that turn is not the turn's.
func (s *Sessions) Told(ctx context.Context, scope string, changes []Change) error {
	return s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, c := range changes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor_files (scope, name, told, seen) VALUES (?1, ?2, ?3, ?3)
				ON CONFLICT (scope, name) DO UPDATE SET
					told = CASE WHEN told = ?4 THEN ?3 ELSE told END,
					seen = CASE WHEN told = ?4 THEN ?3 ELSE told END`, scope, c.Name, c.Now, c.Was); err != nil {
				return fmt.Errorf("supervisor: the user's files: %w", err)
			}
		}
		return nil
	})
}

// Turned is part of Files: the user's files in the directory of thread,
// scope's supervisor thread, that its turn ended at pos changed, as the
// daemon looks once it read that end: those that differ from what it saw
// after the last turn, from what the first launch gave, or from what the
// wake that started the turn told; and the hashes of all of them now, for
// Seen once the line that names them was posted. A file it has never
// looked at is not named, nor is anything for a turn end at or before the
// last one Seen recorded, as one read again after a restart. What it
// returns is stored before it returns, and a turn end read again before
// Seen gets the same, so that the line posted once names what was found
// the first time.
func (s *Sessions) Turned(ctx context.Context, scope string, thread control.ThreadID, pos uint64) ([]string, map[string]string, error) {
	var (
		last    int64
		pending sql.NullString
	)
	if err := s.Core.Store.DB.QueryRowContext(ctx, `SELECT files_pos, files_pending FROM supervisor_wakes WHERE scope = ?`, scope).
		Scan(&last, &pending); err != nil {
		return nil, nil, fmt.Errorf("supervisor: the user's files: %w", err)
	}
	if pos <= uint64(last) {
		return nil, nil, nil
	}
	type found struct {
		Pos    uint64            `json:"pos"`
		Names  []string          `json:"names"`
		Hashes map[string]string `json:"hashes"`
	}
	if pending.Valid {
		var f found
		if err := json.Unmarshal([]byte(pending.String), &f); err == nil && f.Pos == pos {
			return f.Names, f.Hashes, nil
		}
	}
	now, err := s.now(ctx, thread)
	if err != nil {
		return nil, nil, err
	}
	seen, err := s.recorded(ctx, scope, "seen")
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, name := range differ(seen, now) {
		if _, ok := seen[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		b, _ := json.Marshal(found{Pos: pos, Names: names, Hashes: now})
		if _, err := s.Core.Store.DB.ExecContext(ctx, `UPDATE supervisor_wakes SET files_pending = ? WHERE scope = ?`, string(b), scope); err != nil {
			return nil, nil, fmt.Errorf("supervisor: the user's files: %w", err)
		}
	}
	return names, now, nil
}

// Seen is part of Files: the daemon looked at scope's files, as hashed,
// after its supervisor's turn that ended at pos.
func (s *Sessions) Seen(ctx context.Context, scope string, pos uint64, hashed map[string]string) error {
	if hashed == nil {
		return nil
	}
	return s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, name := range UserFiles {
			h, ok := hashed[name]
			if !ok {
				continue
			}
			// A file first seen after a turn was never given (told "").
			if _, err := tx.ExecContext(ctx, `INSERT INTO supervisor_files (scope, name, told, seen) VALUES (?, ?, '', ?)
				ON CONFLICT (scope, name) DO UPDATE SET seen = excluded.seen`, scope, name, h); err != nil {
				return fmt.Errorf("supervisor: the user's files: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE supervisor_wakes SET files_pos = max(files_pos, ?), files_pending = NULL WHERE scope = ?`, int64(pos), scope); err != nil {
			return fmt.Errorf("supervisor: the user's files: %w", err)
		}
		return nil
	})
}

// Files is what a Waker asks about the user's files of its scope (design,
// section 7.1, "Two layers"): before each wake, which changed since the
// session was last given or told them, to name them in the wake; after
// each turn of the supervisor's thread, which changed by its end, to name
// them in a line in its thread. Sessions is one.
type Files interface {
	Changed(ctx context.Context, scope string, thread control.ThreadID) ([]Change, error)
	Told(ctx context.Context, scope string, changes []Change) error
	Turned(ctx context.Context, scope string, thread control.ThreadID, pos uint64) ([]string, map[string]string, error)
	Seen(ctx context.Context, scope string, pos uint64, hashed map[string]string) error
}

var _ Files = (*Sessions)(nil)

// FilesKeyPrefix starts the key of the line that names the user's files a
// supervisor's turn changed; the rest is the position of its turn_ended.
const FilesKeyPrefix = "hataori-files-"

// FilesNotice is the line that names the user's files a turn of the
// supervisor changed (Turned). A file a person edited on the host before a
// turn that a person started, rather than a wake, is named after that turn
// too.
func FilesNotice(names []string) string {
	return "Changed by the end of this turn, in the supervisor's instructions: " + list(names) + "."
}

// FilesChanged is the part of a wake that names the user's files that
// changed since the session was last given or told them.
func FilesChanged(names []string) string {
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
