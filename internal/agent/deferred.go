package agent

// DeferredInputAdapter explicitly supports a complete submitted input in one
// action, IdleOnly, and exact acknowledgement after Written reconstruction.
// Absence refuses the opt-in operation; it changes no ordinary input path.
type DeferredInputAdapter interface {
	CanDeferInput() bool
	ValidateDeferredInput(Actions) error
}

// DeferredInputCodec rechecks current native idle eligibility at the actual
// effect boundary, including a frozen request that the receiver proves absent.
// A previously emitted action alone is not current eligibility evidence.
type DeferredInputCodec interface {
	DeferredInputReady(command [][]byte) bool
}

func CanDeferInput(a Adapter) bool {
	d, ok := a.(DeferredInputAdapter)
	return ok && d.CanDeferInput()
}
