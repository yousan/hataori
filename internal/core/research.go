package core

import "cmp"

// ResearchCapability describes the configured ordinary worker adapter, not
// the supervisor's separately selected confined adapter. Unknown support is
// false. The runner must still check the actual launch's attestation.
func (c *Core) ResearchCapability(scope string) (string, bool) {
	name := cmp.Or(c.Scopes[scope].Agent, c.Agent)
	_, known := c.Agents[name]
	if c.Agents == nil {
		known = name != "" && name == c.Agent
	}
	return name, known && c.CanOmitProjectSettings != nil && c.CanOmitProjectSettings(name)
}
