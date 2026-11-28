package workerproto

import (
	"crypto/sha256"
	"encoding/hex"
)

// FileSource names an adapter-reported path without giving the peer a path
// lookup capability. The resident must discover the same name independently.
func FileSource(path string) string {
	if path == "" {
		return "events"
	}
	if path == "@screen" {
		return "terminal"
	}
	s := sha256.Sum256([]byte(path))
	return "file-" + hex.EncodeToString(s[:])
}
