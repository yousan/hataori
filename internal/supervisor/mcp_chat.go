package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/yousan/hataori/internal/control"
)

func (m *MCP) chat(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var q control.ReadChatRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&q); err != nil {
		return "", fmt.Errorf("bad chat arguments: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("bad chat arguments: expected one object")
	}
	q.Op = "list"
	if name == ToolChatRead {
		q.Op = "read"
		if q.Thread == "" {
			return "", fmt.Errorf("bad chat arguments: thread is required")
		}
	}
	api, err := m.connect()
	if err != nil {
		return "", err
	}
	out, err := api.ReadChat(ctx, q)
	if err != nil {
		return "", err
	}
	// Encode cursors as data so no cursor can introduce an unquoted line.
	b, err := json.Marshal(out)
	return string(b), err
}
