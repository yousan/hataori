package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// waitsRows are the lines that an input_waits event gets, one in each of
// convs, the conversations its thread is shown in.
func (f *FrontEnd) waitsRows(ctx context.Context, e control.Event, convs []surface.Conversation) ([]Row, error) {
	if e.Kind != "input_waits" {
		return nil, nil
	}
	var rows []Row
	for _, conv := range convs {
		line, err := f.waitsLine(ctx, e, conv.Channel)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{Event: e.Pos, Conv: conv, Message: surface.Message{Text: line}})
	}
	return rows, nil
}

// waitsLine is the line e, an input_waits event, gets in channel: that the
// conversation waits, and for which threads. One that is shown elsewhere is
// named by its number alone: who reads this channel may not read that one.
func (f *FrontEnd) waitsLine(ctx context.Context, e control.Event, channel string) (string, error) {
	var d struct{ Running []control.ThreadID }
	json.Unmarshal(e.Data, &d)
	var running []string
	for _, id := range d.Running {
		link, err := f.linkIn(ctx, id, channel)
		if err != nil {
			return "", err
		}
		name := fmt.Sprintf("thread %d", id)
		if link != "" {
			name += " (" + link + ")"
		}
		running = append(running, name)
	}
	return waitsText(running), nil
}

func waitsText(running []string) string {
	return "This waits until another conversation with the supervisor ends its turn. Running: " + strings.Join(running, ", ") + "."
}

// linkIn returns a link to the chat thread in channel that thread is shown
// in, and "" when there is none.
func (f *FrontEnd) linkIn(ctx context.Context, thread control.ThreadID, channel string) (string, error) {
	l, ok := f.Driver.(surface.Linker)
	if !ok {
		return "", nil
	}
	convs, err := f.convsOf(ctx, thread)
	for _, c := range convs {
		if c.Channel == channel {
			return l.Link(c), nil
		}
	}
	return "", err
}
