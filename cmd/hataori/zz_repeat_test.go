package main

import (
	"fmt"
	"testing"
)

// TEMPORARY (#478), dropped before merge: the test 50 times in CI, as
// go test -count=50 -run '^TestConnectTwoAtOnce$' would.
func TestRepeatConnectTwoAtOnce(t *testing.T) {
	for i := range 50 {
		t.Run(fmt.Sprint(i), TestConnectTwoAtOnce)
	}
}
