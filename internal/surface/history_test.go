package surface_test

import (
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
	"testing"
)

func TestHistoryCapabilitiesAreOptional(t *testing.T) {
	var d surface.Driver = &fake.Driver{}
	if _, ok := d.(surface.ThreadLister); ok {
		t.Fatal("fake advertises thread listing")
	}
	if _, ok := d.(surface.MessageReader); ok {
		t.Fatal("fake advertises history reads")
	}
}
