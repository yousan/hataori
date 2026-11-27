package workerproto

import "testing"

func TestAdmissionRequiresNegotiatedPlacementBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		version string
		caps    []string
	}{{"1.2", allCaps()}, {Version, []string{"host", "receipts"}}} {
		local, peer := hello(), hello()
		peer.Version = tc.version
		peer.Capabilities = tc.caps
		c, err := Negotiate(registration, local, peer)
		if err != nil {
			t.Fatal(err)
		}
		m := Message{Kind: "request", ID: "start", Binding: registration, Target: &target, Key: "start", Op: "start", Body: raw(t, Start{AgentConfig: agentConfig, Holder: "own", Workspace: "workspace-A", Admission: &Admission{MaxResident: 1}})}
		data, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Decode(data, &target); err == nil {
			t.Fatal("admission limits would be ignored by the negotiated peer")
		}
	}
}
