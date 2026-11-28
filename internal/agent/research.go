package agent

// ProjectSettingsOmitter declares that ordinary launches can omit repository
// settings and hooks, including on resume. It does no I/O. This permits
// preflight only: the runner still requires each launch to attest
// NoProjectSettings. Confined launches are a separate contract.
type ProjectSettingsOmitter interface {
	CanOmitProjectSettings() bool
}

// CanOmitProjectSettings refuses undeclared support, including a nil adapter.
func CanOmitProjectSettings(a Adapter) bool {
	c, ok := a.(ProjectSettingsOmitter)
	return ok && c.CanOmitProjectSettings()
}
