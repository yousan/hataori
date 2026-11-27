package host

import (
	"context"
	"encoding/json"
	"time"
)

// AttachIdentity verifies the reserved host identity and attaches on that same
// connection. It never follows a stored socket pointer or opens a second
// connection between the identity check and giving terminal authority.
func AttachIdentity(ctx context.Context, stateDir, identity string) (*Attachment, error) {
	c, err := DialIdentity(ctx, stateDir, identity)
	if err != nil {
		return nil, err
	}
	a := &Attachment{TerminalStream: &TerminalStream{conn: c.conn, dec: c.dec}, enc: json.NewEncoder(c.conn)}
	c.conn.SetWriteDeadline(time.Now().Add(followWriteTimeout))
	if err := a.enc.Encode(request{Op: "attach"}); err != nil {
		c.Close()
		return nil, err
	}
	c.conn.SetReadDeadline(time.Now().Add(followFirstTimeout))
	first, err := a.read()
	if err != nil {
		c.Close()
		return nil, err
	}
	c.conn.SetDeadline(time.Time{})
	a.first, a.cols, a.rows = &first, first.Cols, first.Rows
	return a, nil
}
