package supervisor

import (
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// The caller a supervisor's token is bound to, by its scope's autonomy
// (#650): under watch it reads and nothing more, so the core refuses its
// Start, Send and AskInterrupt even without the API that names the mode;
// under propose its starts and sends are proposals; under act they run.
// None of them may answer: a person decides its requests. An autonomy
// that is not one of the three is propose.
func TestCallerOfEachAutonomy(t *testing.T) {
	all := control.OpRead | control.OpStart | control.OpSend | control.OpAsk
	for _, c := range []struct {
		autonomy string
		may      control.Ops
		propose  bool
	}{
		{Watch, control.OpRead, false},
		{Propose, all, true},
		{Act, all, false},
		{"", all, true},
		{"anything", all, true},
	} {
		got := Caller("c1", 7, c.autonomy)
		if got.May != c.may || got.Propose != c.propose {
			t.Errorf("autonomy %q: may %b, propose %v; want %b, %v", c.autonomy, got.May, got.Propose, c.may, c.propose)
		}
		if got.May.Has(control.OpAnswer) || !got.RefuseBusy || got.Mark != Mark || got.AsksIn != 7 || got.From != ID("c1") {
			t.Errorf("autonomy %q: %+v", c.autonomy, got)
		}
	}
}
