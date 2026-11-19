package main

import (
	"fmt"
	"testing"
)

// TEMPORARY (#492), dropped before merge: the test 50 times in CI, as
// go test -count=50 -run '^TestStopButtonStopsAQuietTurn$' would.
func TestRepeatStopButtonStopsAQuietTurn(t *testing.T) {
	for i := range 50 {
		t.Run(fmt.Sprint(i), TestStopButtonStopsAQuietTurn)
	}
}
