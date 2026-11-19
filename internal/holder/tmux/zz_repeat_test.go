package tmux

import (
	"fmt"
	"testing"
)

// TEMPORARY (#497), dropped before merge: the test 50 times in CI, as
// go test -count=50 -run '^TestCapsStopAndNothingChanges$' would.
func TestRepeatCapsStopAndNothingChanges(t *testing.T) {
	for i := range 50 {
		t.Run(fmt.Sprint(i), TestCapsStopAndNothingChanges)
	}
}
