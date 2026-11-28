package fake

import (
	"errors"

	"github.com/yousan/hataori/internal/agent"
)

func (a *Adapter) CanDeferInput() bool { return !a.c.Terminal }

func (*Adapter) ValidateDeferredInput(a agent.Actions) error {
	if len(a.Inputs) != 1 || len(a.Reply) != 0 || len(a.Write) != 1 || len(a.Keys) != 0 {
		return errors.New("fake: deferred input needs one complete submission")
	}
	return nil
}
