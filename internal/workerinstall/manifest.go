// Package workerinstall implements explicit, offline worker binary selection.
// It has no daemon reconnect hooks, service-manager access or session cleanup.
package workerinstall

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const MaxManifest = 64 << 10
const MaxBinary = 512 << 20

// Platform is reported by the worker running the explicit installer, never
// inferred from the main's platform or an artifact filename.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type Artifact struct {
	Platform
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is authenticated as exact bytes by an operator-trusted Ed25519 key.
// A checksum supplied alongside an unauthenticated download is insufficient.
type Manifest struct {
	Format    int        `json:"format"`
	Version   string     `json:"version"`
	Protocol  string     `json:"worker_protocol"`
	Artifacts []Artifact `json:"artifacts"`
}

var releaseID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)
var protocolID = regexp.MustCompile(`^[1-9][0-9]{0,5}\.(0|[1-9][0-9]{0,5})$`)

func supported(p Platform) bool {
	return (p.OS == "linux" || p.OS == "darwin") && (p.Arch == "amd64" || p.Arch == "arm64")
}

// Verify checks provenance, structure, platform and protocol before callers
// create installation directories or read executable bytes.
func Verify(raw, signature []byte, trusted ed25519.PublicKey, platform Platform, protocol string) (Manifest, Artifact, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifest || len(trusted) != ed25519.PublicKeySize || !ed25519.Verify(trusted, raw, signature) {
		return m, Artifact{}, errors.New("worker install: manifest provenance verification failed")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, Artifact{}, errors.New("worker install: invalid manifest")
	}
	if d.Decode(new(any)) != io.EOF || m.Format != 1 || !releaseID.MatchString(m.Version) || !protocolID.MatchString(m.Protocol) || !protocolID.MatchString(protocol) || len(m.Artifacts) == 0 || len(m.Artifacts) > 4 {
		return m, Artifact{}, errors.New("worker install: unsupported manifest")
	}
	if strings.Split(m.Protocol, ".")[0] != strings.Split(protocol, ".")[0] {
		return m, Artifact{}, fmt.Errorf("worker install: incompatible worker protocols: local %s, artifact %s", protocol, m.Protocol)
	}
	if !supported(platform) {
		return m, Artifact{}, errors.New("worker install: unsupported worker platform")
	}
	seen := map[Platform]bool{}
	var chosen Artifact
	for _, a := range m.Artifacts {
		digest, err := hex.DecodeString(a.SHA256)
		if !supported(a.Platform) || seen[a.Platform] || err != nil || len(digest) != 32 || hex.EncodeToString(digest) != a.SHA256 || a.Size <= 0 || a.Size > MaxBinary {
			return m, Artifact{}, errors.New("worker install: invalid or duplicate artifact")
		}
		seen[a.Platform] = true
		if a.Platform == platform {
			chosen = a
		}
	}
	if chosen.Size == 0 {
		return m, Artifact{}, errors.New("worker install: release has no artifact for the worker platform")
	}
	return m, chosen, nil
}
