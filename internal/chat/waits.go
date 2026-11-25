package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yousan/hataori/internal/control"
)

// waitsLine is the line e, an input_waits event, gets in channel: that the
// conversation waits, and for which threads.
func (f *FrontEnd) waitsLine(ctx context.Context, e control.Event, channel string) (string, error) {
	var d struct{ Running []control.ThreadID }
	json.Unmarshal(e.Data, &d)
	var running []string
	for _, id := range d.Running {
		link, err := f.linkTo(ctx, id, channel)
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
