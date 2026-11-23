package supervisor

import (
	_ "embed"
	"strings"
)

//go:embed layer.md
var layerFile string

// Layer is hataori's layer: built in, versioned with hataori, and the
// first part of every launch's instructions (design, section 7.1, "Two
// layers"). Every launch gives it, a wake's included, so it is short. Its
// text is layer.md; testdata/layer.golden fixes it, since a change of it
// changes every launch.
var Layer = strings.TrimSuffix(layerFile, "\n")
