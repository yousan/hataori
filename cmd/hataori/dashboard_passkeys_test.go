package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/instance"
)

// webName is the web listener's name in the passkey tests: passkeys need a
// domain name, and the test's clients reach it on loopback.
const webName = "dash.test"

// testClock is the dashboard server's clock in a test: it moves only when
// the test moves it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// useClock makes the dashboard server of daemons started from now on read
// a clock of the test's own, and returns it.
func useClock(t *testing.T) *testClock {
	c := &testClock{at: time.Now()}
	dashboardNow = c.now
	t.Cleanup(func() { dashboardNow = nil })
	return c
}

// writeNamedCert writes a self-signed certificate for name and 127.0.0.1
// and its key into root's config directory, and returns the hash of its
// public key, which a browser can be told to accept.
func writeNamedCert(t *testing.T, root, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(root, "config", "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(root, "config", "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	c, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return sum[:]
}

// passkeyDash is a dashboard with a web listener named webName and a
// loopback listener, as the page and its browser reach them.
type passkeyDash struct {
	t      *testing.T
	root   string
	web    string // the web listener's address, https://dash.test:port; also its origin
	listen string // where the web listener listens
	hc     *http.Client
	spki   []byte // the hash of the web listener's certificate's key
}

// newPasskeyDash makes an instance root with both listeners. Its daemon is
// not started.
func newPasskeyDash(t *testing.T) *passkeyDash {
	t.Helper()
	root := newRoot(t)
	d := &passkeyDash{t: t, root: root, listen: freePort(t), spki: writeNamedCert(t, root, webName)}
	_, port, _ := net.SplitHostPort(d.listen)
	d.web = "https://" + webName + ":" + port
	withDashboard(t, root, map[string]any{"listen": freePort(t),
		"web": map[string]any{"listen": d.listen, "url": d.web, "tls_cert": "cert.pem", "tls_key": "key.pem"}})
	// The name resolves to the listener, as a browser's would.
	d.hc = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, d.listen)
		}}}
	return d
}

// do sends a request to the web listener, or to base when it is not
// empty, as the page does: the session when s is not empty, the Origin
// when origin is not empty, and body as JSON when it is not nil.
func (d *passkeyDash) do(method, base, path string, s dashSession, origin string, body any) (int, []byte) {
	d.t.Helper()
	hc := d.hc
	if base == "" {
		base = d.web
	} else if strings.HasPrefix(base, "http:") {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, r)
	s.set(req)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if c := resp.Header.Values("Set-Cookie"); len(c) != 0 {
		d.t.Fatalf("%s %s set a cookie: %q", method, path, c)
	}
	return resp.StatusCode, b
}

// post is a POST to the web listener from its own page.
func (d *passkeyDash) post(path string, s dashSession, body any) (int, []byte) {
	d.t.Helper()
	return d.do(http.MethodPost, "", path, s, d.web, body)
}

// signedIn is what a sign-in answers.
type signedInAnswer struct {
	Session  dashSession
	Register *struct {
		Token     string
		ExpiresIn int `json:"expires_in"`
	}
}

// trade opens a sign-in link as the page does and returns its session and
// the registration proof it came with ("" for none).
func (d *passkeyDash) trade(link string) (dashSession, string) {
	d.t.Helper()
	u, _ := url.Parse(link)
	base := u.Scheme + "://" + u.Host
	if code, b := d.do(http.MethodGet, base, u.Path, "", "", nil); code != http.StatusOK {
		d.t.Fatalf("the sign-in page: %d %s", code, b)
	}
	v, _ := url.ParseQuery(u.Fragment)
	hc := d.hc
	if u.Scheme == "http" {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, _ := http.NewRequest(http.MethodGet, base+dashboard.SessionPath, nil)
	req.Header.Set("Authorization", "Link "+v.Get("t"))
	resp, err := hc.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out signedInAnswer
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Session == "" {
		d.t.Fatalf("trading %s: %d %s", base, resp.StatusCode, b)
	}
	if out.Register == nil {
		return out.Session, ""
	}
	if out.Register.ExpiresIn != 300 || out.Register.Token == "" {
		d.t.Fatalf("the registration proof: %s", b)
	}
	return out.Session, out.Register.Token
}

// webLink signs in on the web listener with a fresh link.
func (d *passkeyDash) webLink() (dashSession, string) {
	d.t.Helper()
	return d.trade(signinLink(d.t, d.root, "--web"))
}

// ceremonyAnswer is what a ceremony's beginning answers.
type ceremonyAnswer struct {
	Ceremony string
	Options  json.RawMessage
}

// beginRegister begins a registration with the session s and the proof
// reg, and returns the status and the answer.
func (d *passkeyDash) beginRegister(s dashSession, reg string) (int, ceremonyAnswer, []byte) {
	d.t.Helper()
	code, b := d.post(dashboard.PasskeyRegisterBeginPath, s, map[string]any{"register": reg})
	var a ceremonyAnswer
	json.Unmarshal(b, &a)
	return code, a, b
}

// register registers a new passkey of k with the session s and the proof
// reg, as the page does, and returns the final status, its body and the
// credential; the credential is nil when the beginning was refused.
func (d *passkeyDash) register(k *softKey, s dashSession, reg string) (int, string, *softCred) {
	d.t.Helper()
	code, a, b := d.beginRegister(s, reg)
	if code != http.StatusOK {
		return code, string(b), nil
	}
	c, cred := k.create(d.t, a.Options)
	code, b = d.post(dashboard.PasskeyRegisterFinishPath, s, map[string]any{"ceremony": a.Ceremony, "credential": cred})
	return code, string(b), c
}

// signInWith signs in with the passkey c of k, as the page does, with no
// session, and returns the status, its body and what it answered.
func (d *passkeyDash) signInWith(k *softKey, c *softCred) (int, string, signedInAnswer) {
	d.t.Helper()
	code, b := d.post(dashboard.PasskeySignInBeginPath, "", nil)
	if code != http.StatusOK {
		return code, string(b), signedInAnswer{}
	}
	var a ceremonyAnswer
	json.Unmarshal(b, &a)
	code, b = d.post(dashboard.PasskeySignInFinishPath, "", map[string]any{"ceremony": a.Ceremony, "credential": k.get(d.t, a.Options, c)})
	var out signedInAnswer
	json.Unmarshal(b, &out)
	return code, string(b), out
}

// stored is the passkeys file as the test reads it.
type storedPasskey struct {
	ID        []byte
	Person    string
	Handle    []byte `json:"user_handle"`
	PublicKey []byte `json:"public_key"`
	SignCount uint32 `json:"sign_count"`
	Made      time.Time
	Used      *time.Time
}

// stored reads the passkeys file and checks that only its owner may open
// it.
func (d *passkeyDash) stored() []storedPasskey {
	d.t.Helper()
	p := filepath.Join(d.root, dashboard.PasskeysFile)
	st, err := os.Stat(p)
	if err != nil {
		d.t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		d.t.Fatalf("%s has mode %04o", p, st.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	var f struct{ Passkeys []storedPasskey }
	if err := json.Unmarshal(b, &f); err != nil {
		d.t.Fatalf("%s: %v", b, err)
	}
	return f.Passkeys
}

// checkedAt is when a passkey was checked for session s, as the session
// says; zero for none.
func checkedAt(t *testing.T, s dashSession) time.Time {
	t.Helper()
	p, _, _ := strings.Cut(string(s), ".")
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || len(payload) < 32 {
		t.Fatalf("not a session: %q", s)
	}
	at := binary.BigEndian.Uint64(payload[24:32])
	if at == 0 {
		return time.Time{}
	}
	return time.Unix(int64(at), 0)
}

// threadsWith reports the status of the thread list with session s on the
// web listener.
func (d *passkeyDash) threadsWith(s dashSession) int {
	d.t.Helper()
	code, _ := d.do(http.MethodGet, "", dashboard.ThreadsPath, s, "", nil)
	return code
}

// Right after a fresh sign-in link, the web listener registers a passkey:
// the relying party is its name, user verification is required and the
// credential is discoverable. The passkey then signs its person in with no
// session and no name, and the session records when it was checked. The
// file in the state directory holds its id, public key, counter, person,
// and when it was made and last used, and only the owner may open it.
func TestPasskeyRegisteredRightAfterAFreshLinkSignsIn(t *testing.T) {
	clock := useClock(t)
	d := newPasskeyDash(t)
	startDaemon(t, d.root)
	k := &softKey{rpID: webName, origin: d.web}

	s, reg := d.webLink()
	if reg == "" {
		t.Fatal("a fresh link on the web listener came with no registration proof")
	}
	if c := checkedAt(t, s); !c.IsZero() {
		t.Fatalf("a link's session records a passkey check at %v", c)
	}
	code, a, b := d.beginRegister(s, reg)
	if code != http.StatusOK {
		t.Fatalf("begin: %d %s", code, b)
	}
	var o creationOptions
	json.Unmarshal(a.Options, &o)
	if o.PublicKey.RP.ID != webName || o.PublicKey.Selection.ResidentKey != "required" || o.PublicKey.Selection.UserVerification != "required" {
		t.Fatalf("registration options: %s", a.Options)
	}
	c, cred := k.create(t, a.Options)
	if code, b := d.post(dashboard.PasskeyRegisterFinishPath, s, map[string]any{"ceremony": a.Ceremony, "credential": cred}); code != http.StatusOK {
		t.Fatalf("finish: %d %s", code, b)
	}
	made := clock.now()
	keys := d.stored()
	if len(keys) != 1 || !bytes.Equal(keys[0].ID, c.id) || keys[0].Person != "owner" || len(keys[0].PublicKey) == 0 ||
		!bytes.Equal(keys[0].Handle, c.handle) || keys[0].SignCount != c.count || !keys[0].Made.Equal(made) || keys[0].Used != nil {
		t.Fatalf("stored: %+v", keys)
	}

	clock.add(time.Minute)
	code, body, in := d.signInWith(k, c)
	if code != http.StatusOK || in.Session == "" {
		t.Fatalf("sign-in with the passkey: %d %s", code, body)
	}
	if got := d.threadsWith(in.Session); got != http.StatusOK {
		t.Fatalf("the passkey's session: %d", got)
	}
	if at := checkedAt(t, in.Session); !at.Equal(clock.now().Truncate(time.Second)) {
		t.Fatalf("the session records its passkey check at %v, want %v", at, clock.now())
	}
	keys = d.stored()
	if len(keys) != 1 || keys[0].SignCount != c.count || keys[0].Used == nil || !keys[0].Used.Equal(clock.now()) {
		t.Fatalf("after the sign-in: %+v", keys)
	}
}

// A passkey is registered only with a proof made for it: the session
// alone, a proof of another session, a proof used once, a proof older than
// five minutes and the loopback listener are refused, and so is an
// authenticator that does not verify the user. A passkey checked just now
// gives a proof of its own.
func TestPasskeyRegistrationNeedsAProofMadeForIt(t *testing.T) {
	clock := useClock(t)
	d := newPasskeyDash(t)
	startDaemon(t, d.root)
	k := &softKey{rpID: webName, origin: d.web}
	refused := func(what string, code int, body string) {
		t.Helper()
		if code != http.StatusForbidden {
			t.Fatalf("%s: %d %s, want 403", what, code, body)
		}
	}

	sA, regA := d.webLink()
	sB, _ := d.webLink()
	for _, reg := range []string{"", "made-up"} {
		code, body, _ := d.register(k, sA, reg)
		refused("the session alone, proof "+reg, code, body)
	}
	code, body, _ := d.register(k, sB, regA)
	refused("another session's proof", code, body)
	code, body, first := d.register(k, sA, regA)
	if code != http.StatusOK {
		t.Fatalf("with its proof: %d %s", code, body)
	}
	code, body, _ = d.register(k, sA, regA)
	refused("a proof used once", code, body)

	sC, regC := d.webLink()
	clock.add(dashboard.RegisterWithin)
	code, body, _ = d.register(k, sC, regC)
	refused("a proof five minutes old", code, body)
	sD, regD := d.webLink()
	code, a, b := d.beginRegister(sD, regD)
	if code != http.StatusOK {
		t.Fatalf("begin: %d %s", code, b)
	}
	clock.add(dashboard.RegisterWithin)
	_, cred := (&softKey{rpID: webName, origin: d.web}).create(t, a.Options)
	code, b = d.post(dashboard.PasskeyRegisterFinishPath, sD, map[string]any{"ceremony": a.Ceremony, "credential": cred})
	refused("a ceremony finished five minutes after the link", code, string(b))

	// A passkey just checked gives a proof for its own session, and only
	// for that one.
	code, body, in := d.signInWith(k, first)
	if code != http.StatusOK || in.Register == nil {
		t.Fatalf("sign-in: %d %s", code, body)
	}
	// The second passkey is on another device: an authenticator that holds
	// one of the person's passkeys is excluded.
	k2 := &softKey{rpID: webName, origin: d.web}
	code, body, _ = d.register(k2, sA, in.Register.Token)
	refused("a passkey check's proof with another session", code, body)
	code, body, second := d.register(k2, in.Session, in.Register.Token)
	if code != http.StatusOK {
		t.Fatalf("after a passkey check: %d %s", code, body)
	}
	if keys := d.stored(); len(keys) != 2 || !bytes.Equal(keys[1].ID, second.id) || !bytes.Equal(keys[0].Handle, keys[1].Handle) {
		t.Fatalf("stored: %+v", keys)
	}

	// No user verification, and a credential that cannot be found without
	// a name, are refused.
	for _, bad := range []*softKey{{rpID: webName, origin: d.web, noUV: true}, {rpID: webName, origin: d.web, noRK: true}} {
		s, reg := d.webLink()
		if code, body, _ := d.register(bad, s, reg); code != http.StatusBadRequest {
			t.Fatalf("%+v: %d %s, want 400", bad, code, body)
		}
	}
	if keys := d.stored(); len(keys) != 2 {
		t.Fatalf("a refused registration was kept: %+v", keys)
	}

	// On the loopback listener a link brings no proof, and neither a
	// registration nor a passkey sign-in begins.
	local := signinLink(t, d.root)
	u, _ := url.Parse(local)
	base := "http://" + u.Host
	ls, lreg := d.trade(local)
	if lreg != "" {
		t.Fatal("a loopback link came with a registration proof")
	}
	_, regE := d.webLink()
	for _, path := range []string{dashboard.PasskeyRegisterBeginPath, dashboard.PasskeySignInBeginPath} {
		if code, b := d.do(http.MethodPost, base, path, ls, base, map[string]any{"register": regE}); code != http.StatusForbidden || !strings.Contains(string(b), "web listener") {
			t.Fatalf("%s on loopback: %d %s, want 403", path, code, b)
		}
	}
}

// A passkey whose signature counter goes back opens nothing, keeps its
// counter, and the refusal is logged; a passkey that keeps no counter
// signs in each time.
func TestPasskeyCounterGoingBackIsRefusedAndLogged(t *testing.T) {
	useClock(t)
	d := newPasskeyDash(t)
	logged := startDaemonLog(t, d.root)
	k := &softKey{rpID: webName, origin: d.web}
	s, reg := d.webLink()
	code, body, c := d.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	for range 2 {
		if code, body, _ := d.signInWith(k, c); code != http.StatusOK {
			t.Fatalf("sign-in: %d %s", code, body)
		}
	}
	last := d.stored()[0]
	c.count -= 3 // raised once before it signs
	code, body, in := d.signInWith(k, c)
	if code != http.StatusUnauthorized || in.Session != "" || !strings.Contains(body, "counter") {
		t.Fatalf("a counter gone back: %d %s", code, body)
	}
	if now := d.stored()[0]; now.SignCount != last.SignCount || !now.Used.Equal(*last.Used) {
		t.Fatalf("the refused sign-in changed the passkey: %+v, was %+v", now, last)
	}
	if l := logged(); !strings.Contains(l, "signature counter did not go up") || !strings.Contains(l, "person=owner") {
		t.Fatalf("the refusal is not logged:\n%s", l)
	}

	still := &softKey{rpID: webName, origin: d.web, noCounter: true}
	s, reg = d.webLink()
	code, body, c = d.register(still, s, reg)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	for range 2 {
		if code, body, _ := d.signInWith(still, c); code != http.StatusOK {
			t.Fatalf("a passkey without a counter: %d %s", code, body)
		}
	}
}

// setOwner makes ada and grace the people of root's config, and owner its
// owner: the one person who may sign in to the dashboard.
func setOwner(t *testing.T, root, owner string) {
	t.Helper()
	p := filepath.Join(root, instance.ConfigFile)
	b, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["people"] = []map[string]any{{"name": "ada"}, {"name": "grace"}}
	cfg["owner"] = owner
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A passkey of a person who may no longer sign in opens nothing, and that
// is logged.
func TestPasskeyOfAPersonWhoMayNoLongerSignIn(t *testing.T) {
	useClock(t)
	d := newPasskeyDash(t)
	setOwner(t, d.root, "ada")
	stop := runDaemon(t, d.root)
	k := &softKey{rpID: webName, origin: d.web}
	s, reg := d.webLink()
	code, body, c := d.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	if keys := d.stored(); len(keys) != 1 || keys[0].Person != "ada" {
		t.Fatalf("stored: %+v", keys)
	}
	stop()
	// The owner is grace now: ada may no longer sign in.
	setOwner(t, d.root, "grace")
	_, logged := runDaemonLog(t, d.root)
	before := d.stored()
	code, body, in := d.signInWith(k, c)
	if code != http.StatusUnauthorized || in.Session != "" || in.Register != nil {
		t.Fatalf("a passkey of a person who may no longer sign in: %d %s", code, body)
	}
	if after := d.stored(); after[0].Used != nil || after[0].SignCount != before[0].SignCount {
		t.Fatalf("the refused sign-in changed the passkey: %+v", after)
	}
	if l := logged(); !strings.Contains(l, "may no longer sign in") || !strings.Contains(l, "person=ada") {
		t.Fatalf("the refusal is not logged:\n%s", l)
	}
}

// The dashboard lists the person's passkeys and removes one from either
// listener, and a removed passkey no longer signs in. Removing the file
// removes them all at the next start, and a file others may open stops
// the daemon.
func TestPasskeysListedAndRemovedOnEitherListener(t *testing.T) {
	useClock(t)
	d := newPasskeyDash(t)
	stop, logged := runDaemonLog(t, d.root)
	keys := []*softKey{{rpID: webName, origin: d.web}, {rpID: webName, origin: d.web}} // two devices
	var creds []*softCred
	for _, k := range keys {
		s, reg := d.webLink()
		code, body, c := d.register(k, s, reg)
		if code != http.StatusOK {
			t.Fatalf("register: %d %s", code, body)
		}
		creds = append(creds, c)
	}
	local := signinLink(t, d.root)
	u, _ := url.Parse(local)
	base := "http://" + u.Host
	ls, _ := d.trade(local)
	ws, _ := d.webLink()
	list := func(base string, s dashSession) []string {
		t.Helper()
		code, b := d.do(http.MethodGet, base, dashboard.PasskeysPath, s, "", nil)
		var out []struct {
			ID   string
			Made time.Time
		}
		if code != http.StatusOK || json.Unmarshal(b, &out) != nil {
			t.Fatalf("list on %s: %d %s", base, code, b)
		}
		var ids []string
		for _, x := range out {
			if x.Made.IsZero() {
				t.Fatalf("a passkey listed without when it was made: %s", b)
			}
			ids = append(ids, x.ID)
		}
		return ids
	}
	want := []string{b64(creds[0].id), b64(creds[1].id)}
	for _, at := range []struct {
		base string
		s    dashSession
	}{{base, ls}, {"", ws}} {
		if got := list(at.base, at.s); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("listed on %q: %v, want %v", at.base, got, want)
		}
	}

	if code, b := d.do(http.MethodDelete, base, dashboard.PasskeysPath+"/"+b64(creds[0].id), ls, base, nil); code != http.StatusNoContent {
		t.Fatalf("remove on loopback: %d %s", code, b)
	}
	if code, b := d.do(http.MethodDelete, "", dashboard.PasskeysPath+"/"+b64(creds[0].id), ws, d.web, nil); code != http.StatusNotFound {
		t.Fatalf("remove again: %d %s, want 404", code, b)
	}
	if code, body, _ := d.signInWith(keys[0], creds[0]); code != http.StatusUnauthorized || !strings.Contains(body, "not registered") {
		t.Fatalf("a removed passkey: %d %s", code, body)
	}
	if code, body, _ := d.signInWith(keys[1], creds[1]); code != http.StatusOK {
		t.Fatalf("the other passkey: %d %s", code, body)
	}
	if code, b := d.do(http.MethodDelete, "", dashboard.PasskeysPath+"/"+b64(creds[1].id), ws, d.web, nil); code != http.StatusNoContent {
		t.Fatalf("remove on the web: %d %s", code, b)
	}
	if got := list("", ws); len(got) != 0 {
		t.Fatalf("listed after removing both: %v", got)
	}
	if code, body, _ := d.signInWith(keys[1], creds[1]); code != http.StatusUnauthorized {
		t.Fatalf("a removed passkey: %d %s", code, body)
	}
	if l := logged(); strings.Count(l, "passkey removed") != 2 {
		t.Fatalf("the removals are not logged:\n%s", l)
	}

	// Removing the file removes them all at the next start.
	k := &softKey{rpID: webName, origin: d.web}
	s, reg := d.webLink()
	code, body, c := d.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	stop()
	os.Remove(filepath.Join(d.root, dashboard.PasskeysFile))
	stop, _ = runDaemonLog(t, d.root)
	if code, body, _ := d.signInWith(k, c); code != http.StatusUnauthorized || !strings.Contains(body, "not registered") {
		t.Fatalf("a passkey after its file was removed: %d %s", code, body)
	}
	s, reg = d.webLink()
	if code, body, _ := d.register(&softKey{rpID: webName, origin: d.web}, s, reg); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	stop()
	os.Chmod(filepath.Join(d.root, dashboard.PasskeysFile), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errOut bytes.Buffer
	if code := runCtx(ctx, []string{"run", "--root", d.root}, io.Discard, &errOut); code != exitError || !strings.Contains(errOut.String(), "accessible to other users") {
		t.Fatalf("run with an open passkeys file: exit %d, %q", code, errOut.String())
	}
}

// Besides the operate connection, the passkey routes are the only ones
// that take a request other than GET or HEAD, and each refuses one whose
// Origin is missing, null or not exactly the listener's.
func TestPasskeyRoutesNeedTheirListenersOrigin(t *testing.T) {
	useClock(t)
	d := newPasskeyDash(t)
	startDaemon(t, d.root)
	s, _ := d.webLink()
	local := signinLink(t, d.root)
	u, _ := url.Parse(local)
	base := "http://" + u.Host
	ls, _ := d.trade(local)
	_, port, _ := net.SplitHostPort(d.listen)
	routes := []struct{ method, path string }{
		{http.MethodPost, dashboard.PasskeySignInBeginPath},
		{http.MethodPost, dashboard.PasskeySignInFinishPath},
		{http.MethodPost, dashboard.PasskeyRegisterBeginPath},
		{http.MethodPost, dashboard.PasskeyRegisterFinishPath},
		{http.MethodDelete, dashboard.PasskeysPath + "/AAAA"},
	}
	for _, r := range routes {
		for _, origin := range []string{"", "null", "https://evil.example", "https://" + webName, "https://" + webName + ":1", "http://" + webName + ":" + port, "https://127.0.0.1:" + port} {
			if code, b := d.do(r.method, "", r.path, s, origin, map[string]any{}); code != http.StatusForbidden || !strings.Contains(string(b), "origin") {
				t.Errorf("%s %s with Origin %q: %d %s, want 403", r.method, r.path, origin, code, b)
			}
		}
		if code, b := d.do(r.method, "", r.path, s, d.web, map[string]any{}); code == http.StatusForbidden && strings.Contains(string(b), "origin") {
			t.Errorf("%s %s with its own Origin: %d %s", r.method, r.path, code, b)
		}
	}
	// On the loopback listener too.
	if code, b := d.do(http.MethodDelete, base, dashboard.PasskeysPath+"/AAAA", ls, "", nil); code != http.StatusForbidden {
		t.Errorf("DELETE on loopback without an Origin: %d %s", code, b)
	}
	if code, b := d.do(http.MethodDelete, base, dashboard.PasskeysPath+"/AAAA", ls, base, nil); code != http.StatusNotFound {
		t.Errorf("DELETE on loopback with its Origin: %d %s, want 404", code, b)
	}
	// Every other route, and other methods on these, take GET and HEAD
	// only, with the listener's own Origin too.
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, dashboard.ThreadsPath}, {http.MethodPost, dashboard.SessionPath}, {http.MethodPost, dashboard.PasskeysPath},
		{http.MethodPut, dashboard.PasskeysPath + "/AAAA"}, {http.MethodGet, dashboard.PasskeySignInBeginPath}, {http.MethodDelete, dashboard.PasskeysPath},
		{http.MethodPatch, dashboard.PasskeyRegisterBeginPath}, {http.MethodDelete, dashboard.PasskeySignInBeginPath},
	} {
		code, b := d.do(r.method, "", r.path, s, d.web, map[string]any{})
		if r.method == http.MethodGet {
			if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
				t.Errorf("%s %s: %d %s", r.method, r.path, code, b)
			}
			continue
		}
		if code != http.StatusMethodNotAllowed && !(r.path == dashboard.PasskeySignInBeginPath && code == http.StatusNotFound) {
			t.Errorf("%s %s: %d %s, want 405", r.method, r.path, code, b)
		}
	}
}

// press clicks the element sel names as a person would: with a user
// gesture, which a browser may need before it opens a passkey prompt.
func (b *browser) press(sel string) {
	b.t.Helper()
	b.call("Runtime.evaluate", map[string]any{"expression": "document.querySelector(" + js(sel) + ").click()", "userGesture": true})
}

// virtualCredential is a credential of the browser's virtual
// authenticator.
type virtualCredential struct {
	CredentialID         string `json:"credentialId"` // base64
	IsResidentCredential bool
	RPID                 string `json:"rpId"`
}

// In a browser with a virtual authenticator, on the web listener: right
// after a fresh link the page offers to add a passkey and adds one, for
// the listener's name and discoverable; signed out, the page signs in
// with it and asks for no name; the list shows it and removes it, and
// then it signs nobody in.
func TestPasskeysInTheBrowser(t *testing.T) {
	d := newPasskeyDash(t)
	startDaemon(t, d.root)
	startThread(t, d.root, "t1", "reply with a passkey")
	b := newBrowser(t, "--host-resolver-rules=MAP "+webName+" 127.0.0.1",
		"--ignore-certificate-errors-spki-list="+base64.StdEncoding.EncodeToString(d.spki))
	b.call("WebAuthn.enable", map[string]any{})
	var va struct {
		AuthenticatorID string `json:"authenticatorId"`
	}
	json.Unmarshal(b.call("WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
		"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
		"isUserVerified": true, "automaticPresenceSimulation": true}}), &va)
	credentials := func() []virtualCredential {
		t.Helper()
		var r struct{ Credentials []virtualCredential }
		json.Unmarshal(b.call("WebAuthn.getCredentials", map[string]any{"authenticatorId": va.AuthenticatorID}), &r)
		return r.Credentials
	}
	thread := `[...document.querySelectorAll('[data-section=threads] .row')].some(r => r.textContent.includes('reply with a passkey'))`
	signOut := func() {
		t.Helper()
		b.eval(`localStorage.removeItem('hataori-session'), true`, nil)
		b.call("Page.navigate", map[string]any{"url": d.web + "/"})
		b.until("the offer to sign in with a passkey", `!!document.querySelector('.signed-out [data-passkey=signin]')`)
	}

	b.call("Page.navigate", map[string]any{"url": signinLink(t, d.root, "--web")})
	b.until("the thread, signed in with the link", thread)
	b.until("the offer to add a passkey", `!!document.querySelector('[data-passkey=offer] [data-passkey=register]')`)
	b.press("[data-passkey=register]")
	b.until("the passkey added", `document.querySelector('[data-passkey=offer]')?.textContent.includes('Passkey added')`)
	creds := credentials()
	keys := d.stored()
	if len(creds) != 1 || !creds[0].IsResidentCredential || creds[0].RPID != webName || len(keys) != 1 ||
		base64.StdEncoding.EncodeToString(keys[0].ID) != creds[0].CredentialID {
		t.Fatalf("the authenticator holds %+v; the dashboard keeps %+v", creds, keys)
	}

	signOut()
	b.press("[data-passkey=signin]")
	b.until("the thread, signed in with the passkey", thread)
	var session string
	b.eval(`localStorage.getItem('hataori-session') || ''`, &session)
	if checkedAt(t, dashSession(session)).IsZero() {
		t.Fatal("the passkey's session records no passkey check")
	}
	if keys := d.stored(); keys[0].Used == nil {
		t.Fatalf("the sign-in is not recorded: %+v", keys)
	}

	b.press("[data-tool=passkeys]")
	b.until("the passkey listed", `!!document.querySelector(`+js(`dialog.passkeys[open] [data-passkey-id="`+b64(keys[0].ID)+`"] [data-passkey=remove]`)+`)`)
	b.press("dialog.passkeys [data-passkey=remove]")
	b.until("no passkey listed", `!!document.querySelector('dialog.passkeys [data-passkeys=none]')`)
	if keys := d.stored(); len(keys) != 0 {
		t.Fatalf("the removed passkey is kept: %+v", keys)
	}

	signOut()
	b.press("[data-passkey=signin]")
	b.until("the removed passkey refused", `!!document.querySelector('.signed-out [data-passkey-error=refused]')`)
}
