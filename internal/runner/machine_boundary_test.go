package runner

import "testing"

func TestUnknownMachineOwnershipNeverFallsBackToLocal(t *testing.T) {
	f := newFixture(t, options{})
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	_, routed, err := f.run.remoteDir(StateDir(f.root, 1))
	if !routed || err == nil {
		t.Fatalf("unknown ownership became local: routed=%v err=%v", routed, err)
	}
}
