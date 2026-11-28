package workerinstall

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

func signedManifest(t *testing.T, change func(*Manifest)) ([]byte, []byte, ed25519.PublicKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Format: 1, Version: "v1.0.0", Protocol: "1.3"}
	for _, os := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			m.Artifacts = append(m.Artifacts, Artifact{Platform: Platform{os, arch}, SHA256: strings.Repeat("ab", 32), Size: 10})
		}
	}
	if change != nil {
		change(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw, ed25519.Sign(key, raw), pub
}

func TestVerifySelectsWorkerPlatform(t *testing.T) {
	raw, signature, pub := signedManifest(t, nil)
	for _, os := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			p := Platform{os, arch}
			m, a, err := Verify(raw, signature, pub, p, "1.4")
			if err != nil || a.Platform != p || m.Version != "v1.0.0" {
				t.Fatalf("%v: %v %v %v", p, m, a, err)
			}
		}
	}
}

func TestVerifyRefusesBeforeInstallation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"major", func(m *Manifest) { m.Protocol = "2.0" }},
		{"malformed_protocol", func(m *Manifest) { m.Protocol = "1" }},
		{"path_version", func(m *Manifest) { m.Version = "../old" }},
		{"missing_platform", func(m *Manifest) { m.Artifacts = m.Artifacts[2:] }},
		{"duplicate_platform", func(m *Manifest) { m.Artifacts[1] = m.Artifacts[0] }},
		{"bad_digest", func(m *Manifest) { m.Artifacts[0].SHA256 = "ab" }},
		{"oversize", func(m *Manifest) { m.Artifacts[0].Size = MaxBinary + 1 }},
		{"format", func(m *Manifest) { m.Format++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, signature, pub := signedManifest(t, tc.change)
			_, _, err := Verify(raw, signature, pub, Platform{"linux", "amd64"}, "1.3")
			if err == nil {
				t.Fatal("accepted invalid release")
			}
			if tc.name == "major" && (!strings.Contains(err.Error(), "local 1.3") || !strings.Contains(err.Error(), "artifact 2.0")) {
				t.Fatalf("missing version diagnostics: %v", err)
			}
		})
	}
	raw, signature, pub := signedManifest(t, nil)
	for _, bad := range []struct{ raw, signature, key []byte }{
		{append(append([]byte{}, raw...), ' '), signature, pub},
		{raw, make([]byte, 64), pub},
		{raw, signature, make([]byte, 32)},
		{raw, signature, nil},
	} {
		if _, _, err := Verify(bad.raw, bad.signature, bad.key, Platform{"linux", "amd64"}, "1.3"); err == nil {
			t.Fatal("accepted unauthenticated manifest")
		}
	}
}
