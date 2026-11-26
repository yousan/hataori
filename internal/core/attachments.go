package core

import (
	"context"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

func (c *Core) resolveAttachments(ctx context.Context, sid int64, from, key string, in control.Input) (control.Input, error) {
	if len(in.Attachments) == 0 {
		return in, nil
	}
	var confined bool
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT t.confined FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE s.id=?`, sid).Scan(&confined); err != nil {
		return in, err
	}
	if confined {
		for _, f := range in.Attachments {
			in.Files = append(in.Files, control.InputFile{Name: f.Name, Reason: "attachments for confined sessions are not enabled"})
		}
		in.Attachments = nil
		return in, nil
	}
	client, _, _ := people.Split(from)
	var files []control.InputFile
	var err error
	if c.Attachments != nil {
		files, err = c.Attachments.Resolve(ctx, sid, client, key, in.Attachments)
	} else {
		for _, f := range in.Attachments {
			files = append(files, control.InputFile{Name: f.Name, Reason: "attachment storage is unavailable"})
		}
	}
	if err != nil {
		return in, err
	}
	in.Files, in.Attachments = files, nil
	return in, nil
}
