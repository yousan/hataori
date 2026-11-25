package agent

import (
	"context"
	"testing"
)

type plainAdapter struct{}

func (plainAdapter) Probe(context.Context, Spec) (Health, error) { return Health{}, nil }
func (plainAdapter) Launch(Spec) (Launch, error)                 { return Launch{}, nil }
func (plainAdapter) NewCodec(Spec) Codec                         { return nil }

// An adapter without a login flow is still an Adapter, and the core finds
// out by a type assertion.
func TestLoginFlowIsOptional(t *testing.T) {
	var a Adapter = plainAdapter{}
	if _, ok := a.(LoginFlow); ok {
		t.Fatal("plainAdapter reported a login flow")
	}
}
