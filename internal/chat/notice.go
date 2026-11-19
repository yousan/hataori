package chat

import (
	"context"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// Notice puts text in each conversation of thread id here, once for key:
// a key it was given before, also before a restart, posts nothing. Its
// row is keyed by event 0 and the key's place in chat_notices. A thread
// shown in no conversation of this front end gets nothing.
func (f *FrontEnd) Notice(ctx context.Context, id control.ThreadID, key, text string) error {
	convs, err := f.convsOf(ctx, id)
	if err != nil || len(convs) == 0 {
		return err
	}
	err = f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		var rows []Row
		for _, conv := range convs {
			res, err := tx.ExecContext(ctx, `INSERT INTO chat_notices (client, channel, message, received_at)
				VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, conv.Channel, "notice:"+key, time.Now().UnixMilli())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // told before
			}
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT rowid FROM chat_notices WHERE client = ? AND channel = ? AND message = ?`,
				f.Name, conv.Channel, "notice:"+key).Scan(&n); err != nil {
				return err
			}
			rows = append(rows, Row{Event: 0, Part: -(1 << 40) - int(n), Conv: conv, Message: surface.Message{Text: text}})
		}
		return f.Outbox.WriteTx(ctx, tx, rows...)
	})
	if err != nil {
		return err
	}
	f.Outbox.Wake()
	return nil
}
