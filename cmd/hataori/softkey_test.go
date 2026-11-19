package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// softKey is a software authenticator and the part of a browser that
// talks to it: it answers a registration's or a sign-in's options as
// navigator.credentials would on the page at origin, with "none"
// attestation and ES256 keys. Its CBOR and signatures are written here,
// apart from the library the server checks them with.
type softKey struct {
	rpID, origin string
	// noUV makes it answer without verifying the user; noRK says, as a
	// browser's credProps does, that the credential is not discoverable;
	// noCounter makes credentials that keep no signature counter, as
	// synced passkeys do.
	noUV, noRK, noCounter bool
	creds                 []*softCred
}

// softCred is one credential the authenticator holds.
type softCred struct {
	id     []byte
	key    *ecdsa.PrivateKey
	handle []byte // the user handle it was made for
	// count is its signature counter, raised before each signature when
	// counting is set; a test sets it to make the counter go back.
	count    uint32
	counting bool
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("%q is not base64url: %v", s, err)
	}
	return b
}

// CBOR, as much as an attestation object and a COSE key need.
func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
	return []byte{major<<5 | 26, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func cborInt(v int) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }
func cborMap(n int) []byte      { return cborHead(5, uint64(n)) }
func cat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// coseKey is pub as a COSE_Key: EC2, ES256, P-256.
func coseKey(pub *ecdsa.PublicKey) []byte {
	raw, _ := pub.Bytes() // 0x04 | x | y
	return cat(cborMap(5), cborInt(1), cborInt(2), cborInt(3), cborInt(-7), cborInt(-1), cborInt(1),
		cborInt(-2), cborBytes(raw[1:33]), cborInt(-3), cborBytes(raw[33:65]))
}

// Authenticator data flags.
const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

func (k *softKey) authData(flags byte, count uint32, attested []byte) []byte {
	rp := sha256.Sum256([]byte(k.rpID))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], count)
	f := flags | flagUP
	if !k.noUV {
		f |= flagUV
	}
	return cat(rp[:], []byte{f}, c[:], attested)
}

func (k *softKey) clientData(kind, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

// creationOptions is the part of a registration's options a browser reads.
type creationOptions struct {
	PublicKey struct {
		RP        struct{ ID, Name string }
		User      struct{ ID, Name, DisplayName string }
		Challenge string
		Exclude   []struct{ ID string } `json:"excludeCredentials"`
		Selection struct {
			ResidentKey      string `json:"residentKey"`
			UserVerification string `json:"userVerification"`
		} `json:"authenticatorSelection"`
		Extensions map[string]any
	}
}

// create answers a registration's options with a new credential, as
// navigator.credentials.create does: the credential, and the
// PublicKeyCredential's JSON. It fails the test when a browser would
// refuse the options: another relying party, or a credential it holds
// already among those excluded.
func (k *softKey) create(t *testing.T, raw json.RawMessage) (*softCred, json.RawMessage) {
	t.Helper()
	var o creationOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("registration options %s: %v", raw, err)
	}
	if o.PublicKey.RP.ID != k.rpID {
		t.Fatalf("the relying party is %q, not the page's %q", o.PublicKey.RP.ID, k.rpID)
	}
	for _, x := range o.PublicKey.Exclude {
		for _, c := range k.creds {
			if x.ID == b64(c.id) {
				t.Fatalf("this authenticator holds an excluded credential")
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &softCred{id: make([]byte, 32), key: key, handle: unb64(t, o.PublicKey.User.ID), counting: !k.noCounter}
	rand.Read(c.id)
	if c.counting {
		c.count++
	}
	attested := cat(make([]byte, 16), []byte{0, byte(len(c.id))}, c.id, coseKey(&key.PublicKey))
	att := cat(cborMap(3), cborText("fmt"), cborText("none"), cborText("attStmt"), cborMap(0),
		cborText("authData"), cborBytes(k.authData(flagAT, c.count, attested)))
	ext := map[string]any{}
	if _, ok := o.PublicKey.Extensions["credProps"]; ok {
		ext["credProps"] = map[string]any{"rk": !k.noRK}
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64(c.id), "rawId": b64(c.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64(k.clientData("webauthn.create", o.PublicKey.Challenge)),
			"attestationObject": b64(att),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": ext,
	})
	k.creds = append(k.creds, c)
	return c, out
}

// get answers a sign-in's options with c, as navigator.credentials.get
// does when the person picks c, and returns the PublicKeyCredential's
// JSON.
func (k *softKey) get(t *testing.T, raw json.RawMessage, c *softCred) json.RawMessage {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge        string
			RPID             string `json:"rpId"`
			UserVerification string `json:"userVerification"`
			Allow            []any  `json:"allowCredentials"`
		}
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("sign-in options %s: %v", raw, err)
	}
	if o.PublicKey.RPID != k.rpID {
		t.Fatalf("the relying party is %q, not the page's %q", o.PublicKey.RPID, k.rpID)
	}
	if c.counting {
		c.count++
	}
	data := k.authData(0, c.count, nil)
	cd := k.clientData("webauthn.get", o.PublicKey.Challenge)
	sum := sha256.Sum256(cd)
	digest := sha256.Sum256(cat(data, sum[:]))
	sig, err := ecdsa.SignASN1(rand.Reader, c.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id": b64(c.id), "rawId": b64(c.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64(cd),
			"authenticatorData": b64(data),
			"signature":         b64(sig),
			"userHandle":        b64(c.handle),
		},
		"clientExtensionResults": map[string]any{},
	})
	return out
}
