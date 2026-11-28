package agent

// DeferredInputAdapter explicitly supports a complete submitted input in one
// action, IdleOnly, and exact acknowledgement after Written reconstruction.
// Absence refuses the opt-in operation; it changes no ordinary input path.
type DeferredInputAdapter interface {
	CanDeferInput() bool
	ValidateDeferredInput(Actions) error
}

func CanDeferInput(a Adapter) bool {
	d, ok := a.(DeferredInputAdapter)
	return ok && d.CanDeferInput()
}
