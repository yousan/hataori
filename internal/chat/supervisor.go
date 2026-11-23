package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// A supervisor's thread (design, section 7.1, "What it posts") is a
// conversation, so the front end posts in it differently from a worker
// thread: a completed turn pings nobody (#639); a turn that a wake started
// posts its final answer only, and nothing for "(quiet)"; and a person is
// mentioned only where the agent wrote "@<name>" (#651). The front end
// tells such a thread by its being confined: only a supervisor's thread is
// started confined, and a thread's confinement never changes.

// svThread is what the front end looked up about a thread.
type svThread struct {
	sv    bool   // a supervisor's thread
	scope string // its scope
}

// supervisors reports whether thread is a supervisor's, remembering the
// answer in r. A thread the store does not have is none.
func (f *FrontEnd) supervisors(ctx context.Context, r *reader, thread control.ThreadID) (bool, error) {
	t, err := f.svThread(ctx, r, thread)
	return t.sv, err
}

func (f *FrontEnd) svThread(ctx context.Context, r *reader, thread control.ThreadID) (svThread, error) {
	if t, ok := r.sv[thread]; ok {
		return t, nil
	}
	var t svThread
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT confined, scope FROM threads WHERE id = ?`, int64(thread)).Scan(&t.sv, &t.scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return svThread{}, fmt.Errorf("chat: whether thread %d is a supervisor's: %w", thread, err)
	}
	if r.sv == nil {
		r.sv = map[control.ThreadID]svThread{}
	}
	r.sv[thread] = t
	return t, nil
}

// wakeKeyPrefix starts the key of every wake the daemon gives a
// supervisor's thread, a reminder's included (supervisor.WakeKeyPrefix).
const wakeKeyPrefix = "hataori-wake-"

// quietAnswer is the final answer of a wake's turn that posts nothing.
const quietAnswer = "(quiet)"

// woke reports whether a turn with key was started by a wake.
func woke(key string) bool { return strings.HasPrefix(key, wakeKeyPrefix) }

// mentions returns the ids, on this front end's service, of the people
// that text names with "@<name>", where name is a person who may act in
// scope: the owner, or one of the scope's people. A name that is no such
// person, or one with no id on this service, mentions nobody.
func (f *FrontEnd) mentions(scope, text string) []string {
	if !strings.Contains(text, "@") {
		return nil
	}
	names := f.People.Of(scope)
	// The longest name that fits wins: "@ann.smith" names ann.smith, not
	// also ann.
	slices.SortStableFunc(names, func(a, b string) int { return len(b) - len(a) })
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if before, _ := utf8.DecodeLastRuneInString(text[:i]); i > 0 && nameRune(before) {
			continue // as in an email address
		}
		for _, name := range names {
			end := i + 1 + len(name)
			if !strings.HasPrefix(text[i+1:], name) {
				continue
			}
			if after, _ := utf8.DecodeRuneInString(text[end:]); end < len(text) && nameRune(after) {
				continue // a longer word
			}
			for _, id := range f.People.IDs(name, f.Name) {
				if !slices.Contains(out, id) {
					out = append(out, id)
				}
			}
			break
		}
	}
	return out
}

// nameRune reports whether r can be part of a name next to "@<name>".
func nameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}
