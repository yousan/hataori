package supervisor

import (
	"context"
	"os"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func writeMark(ctx context.Context, q store.Rows, thread control.ThreadID, key *string, autonomy string, root *os.Root, parts []string) error {
	marks, err := store.BoundMarks(ctx, q, thread, key)
	if err != nil || len(marks) == 0 {
		return err
	}
	if autonomy == Act {
		return refusal("a marked conversation writes no files while act is configured; start an unmarked conversation in another chat thread")
	}
	protected := userFile(root, strings.Join(parts, "/")) != ""
	if len(parts) == 1 {
		for _, name := range UserFiles {
			protected = protected || strings.EqualFold(parts[0], name)
		}
	}
	if protected {
		return refusal("a marked conversation changes none of the user's files; start an unmarked conversation in another chat thread")
	}
	return nil
}
