package registry

import "testing"

func TestRegisterLookupDuplicate(t *testing.T) {
	var r Registry[int]
	if err := r.Register("one", 1); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if v, ok := r.Lookup("one"); !ok || v != 1 {
		t.Fatalf("Lookup(one) = %v, %v; want 1, true", v, ok)
	}
	if _, ok := r.Lookup("two"); ok {
		t.Fatal("Lookup(two) found a value that was never registered")
	}
	if err := r.Register("one", 2); err == nil {
		t.Fatal("registering a name twice succeeded")
	}
	if v, _ := r.Lookup("one"); v != 1 {
		t.Fatalf("second Register replaced the value: got %v", v)
	}
}

func TestRegisterRejectsBadNames(t *testing.T) {
	var r Registry[int]
	for _, name := range []string{"", "a/b", "..", "Upper", "1st", "a b"} {
		if err := r.Register(name, 0); err == nil {
			t.Errorf("Register(%q) succeeded", name)
		}
	}
}
