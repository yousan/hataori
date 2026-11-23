package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// A supervisor's thread (design, section 7.1, "What it posts") is a
// conversation, so the front end posts in it differently from a worker
// thread: a completed turn pings nobody (#639). The front end tells such a
// thread by its being confined: only a supervisor's thread is started
// confined, and a thread's confinement never changes.

// supervisors reports whether thread is a supervisor's, remembering the
// answer in r. A thread the store does not have is none.
func (f *FrontEnd) supervisors(ctx context.Context, r *reader, thread control.ThreadID) (bool, error) {
	if sv, ok := r.sv[thread]; ok {
		return sv, nil
	}
	var confined bool
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT confined FROM threads WHERE id = ?`, int64(thread)).Scan(&confined)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("chat: whether thread %d is a supervisor's: %w", thread, err)
	}
	if r.sv == nil {
		r.sv = map[control.ThreadID]bool{}
	}
	r.sv[thread] = confined
	return confined, nil
}
