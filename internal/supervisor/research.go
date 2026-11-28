package supervisor

// ResearchCapability is trusted loaded adapter metadata, never permission
// from a user's files. Sessions derives it again at launch/config checks.
type ResearchCapability struct {
	Agent     string
	Supported bool
}

func (s *Sessions) researchScope(scope string, sv Scope) Scope {
	sv.WorkerResearch.Agent, sv.WorkerResearch.Supported = s.Core.ResearchCapability(scope)
	return sv
}
