package surface

import (
	"os"
	"testing"
)

func TestRegisterLookup(t *testing.T) {
	f := func(*os.Root, []byte) (Driver, error) { return nil, nil }
	if err := Register("test-surface", f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got, ok := Lookup("test-surface"); !ok || got == nil {
		t.Fatal("Lookup did not find the registered factory")
	}
	if err := Register("test-surface", f); err == nil {
		t.Fatal("registering a name twice succeeded")
	}
}
