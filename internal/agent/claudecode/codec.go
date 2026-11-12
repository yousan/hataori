package claudecode

import "github.com/yousan/hataori/internal/agent"

// NewCodec returns the translator for one running session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec { return &codec{spec: spec} }

// codec is filled in by the issues that decode hook calls and the
// transcript; until then it does nothing.
type codec struct{ spec agent.Spec }

func (c *codec) Open() Actions                        { return Actions{} }
func (c *codec) Decode(agent.Source, []byte) Actions  { return Actions{} }
func (c *codec) Input(agent.Input) (Actions, error)   { return Actions{}, nil }
func (c *codec) Answer(agent.Answer) (Actions, error) { return Actions{}, nil }
func (c *codec) Interrupt() Actions                   { return Actions{} }

// Actions is agent.Actions.
type Actions = agent.Actions
