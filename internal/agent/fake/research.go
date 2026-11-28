package fake

// CanOmitProjectSettings follows the fixture's explicit settings behavior.
func (a *Adapter) CanOmitProjectSettings() bool { return !a.c.KeepsSettings }
