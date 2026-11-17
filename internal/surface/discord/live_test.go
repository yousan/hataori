package discord

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// liveDriver is the driver against the real service, for the checks that
// run only when asked for:
//
//	HATAORI_REAL_DISCORD=1 HATAORI_DISCORD_TOKEN_FILE=... HATAORI_DISCORD_BOT=<id> \
//	HATAORI_DISCORD_CHANNEL=<test channel> go test -run TestLive -v ./internal/surface/discord
//
// They post only into the test channel, in threads they open, and delete
// what they post.
func liveDriver(t *testing.T) (*Driver, string) {
	if os.Getenv("HATAORI_REAL_DISCORD") != "1" {
		t.Skip("set HATAORI_REAL_DISCORD=1 to run against the real service")
	}
	b, err := os.ReadFile(os.Getenv("HATAORI_DISCORD_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	id := snowflake.MustParse(os.Getenv("HATAORI_DISCORD_BOT"))
	d := newDriver(id, strings.TrimSpace(string(b)), "https://discord.com/api/v10", "", nil)
	return d, os.Getenv("HATAORI_DISCORD_CHANNEL")
}

func TestLivePosting(t *testing.T) {
	d, ch := liveDriver(t)
	ctx := context.Background()
	conv, err := d.OpenThread(ctx, surface.Conversation{Channel: ch}, "[hataori test] live posting")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		th, _ := snowflake.Parse(conv.Thread)
		if err := d.client.Do(rest.DeleteChannel.Compile(nil, th), nil, nil); err != nil {
			t.Logf("the test thread was not deleted: %v", err)
		}
		// A thread opened without a message leaves a notice (type 18) in the
		// channel, which its author may delete.
		var msgs []wireMessage
		c, _ := snowflake.Parse(ch)
		d.client.Do(rest.GetMessages.Compile(map[string]any{"limit": 10}, c), nil, &msgs)
		for _, m := range msgs {
			if m.Type == 18 && m.Author.ID == d.botID && m.MessageReference != nil && m.MessageReference.ChannelID == th {
				if err := d.Delete(ctx, surface.MessageRef{Conv: surface.Conversation{Channel: ch}, ID: m.ID.String()}); err != nil {
					t.Logf("the thread's notice was not deleted: %v", err)
				}
			}
		}
	})
	key := fmt.Sprintf("live-%d", time.Now().UnixNano()) // a nonce is remembered for minutes
	file := t.TempDir() + "/note.txt"
	os.WriteFile(file, []byte("hello\n"), 0o600)
	text := "[hataori test] a long post\n```sh\n" + strings.Repeat("echo hello\n", 300) + "```\nthe end"
	ref, err := d.Post(ctx, conv, key, surface.Message{Text: text, Files: []surface.File{{Name: "note.txt", Path: file}}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.Post(ctx, conv, key, surface.Message{Text: text})
	if err != nil || again != ref {
		t.Fatalf("the same key posted again: %v %v", again, err)
	}
	if err := d.Edit(ctx, ref, surface.Message{Text: "[hataori test] edited"}); err != nil {
		t.Fatal(err)
	}
	// SetThread is not run here: a rename leaves a notice in the thread
	// that the bot cannot delete, and then it cannot delete the thread.
	var msgs []wireMessage
	th, _ := snowflake.Parse(conv.Thread)
	if err := d.client.Do(rest.GetMessages.Compile(map[string]any{"limit": 10}, th), nil, &msgs); err != nil {
		t.Fatal(err)
	}
	posted := 0
	for _, m := range msgs {
		if m.Author.ID == d.botID && m.Type == typeDefault {
			posted++
			if err := d.Delete(ctx, surface.MessageRef{Conv: conv, ID: m.ID.String()}); err != nil {
				t.Error(err)
			}
		}
	}
	if want := len(split(text, maxText)); posted != want || want < 2 {
		t.Fatalf("%d messages posted, want %d parts", posted, want)
	}
	if err := d.Delete(ctx, ref); err != nil {
		t.Fatalf("deleting again: %v", err)
	}
}
