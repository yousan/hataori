package dashboard

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/yousan/hataori/internal/control"
)

// Passkeys on the web listener (design, section 7.2, "Passkeys on the
// web"). The relying party is the web listener's name. A passkey asks for
// user verification and is discoverable, so signing in asks for no name.
//
// A passkey is registered only with a proof made for that registration: a
// sign-in link was just used, or a passkey the person has was just
// checked. Either gives the page a registration token, bound to the
// session it came with and good for RegisterWithin, which one registration
// uses up. A session alone, however new, adds no passkey, so whoever takes
// a session cannot keep a way back in.
//
// On the loopback listener SSH or the host is already the proof: it
// neither signs in with a passkey nor registers one. Either listener lists
// the person's passkeys and removes one. Besides the operate connection,
// these are the only routes that take a request other than GET or HEAD
// (changeRoute), and each such request must carry the listener's exact
// Origin.
//
//	POST   /api/passkeys/signin/begin       {"ceremony", "options"}; no session needed
//	POST   /api/passkeys/signin/finish      {"ceremony", "credential"} → {"session", "register"}; no session needed
//	POST   /api/passkeys/register/begin     {"register"} → {"ceremony", "options"}
//	POST   /api/passkeys/register/finish    {"ceremony", "credential"} → {"passkey"}
//	GET    /api/passkeys                    [{"id", "made", "used"}], the session's person's
//	DELETE /api/passkeys/{id}               removes one of them
//
// "options" is what the page passes to navigator.credentials, and
// "credential" the PublicKeyCredential it got, both in the JSON forms of
// WebAuthn level 3 (base64url for binary values).
const (
	PasskeysPath              = "/api/passkeys"
	PasskeySignInBeginPath    = "/api/passkeys/signin/begin"
	PasskeySignInFinishPath   = "/api/passkeys/signin/finish"
	PasskeyRegisterBeginPath  = "/api/passkeys/register/begin"
	PasskeyRegisterFinishPath = "/api/passkeys/register/finish"
)

// RegisterWithin is how long after a sign-in link is used, or a passkey is
// checked, a passkey may be registered with that proof.
const RegisterWithin = 5 * time.Minute

const (
	// ceremonyTTL is how long a sign-in ceremony may take.
	ceremonyTTL = 5 * time.Minute
	// maxCeremonies is how many ceremonies may be under way at once: a
	// sign-in begins without a session.
	maxCeremonies = 256
	// maxPasskeyBody is the largest request body a passkey route reads.
	maxPasskeyBody = 64 << 10
)

// changeRoute reports whether method and path name a route that takes a
// request other than GET or HEAD.
func changeRoute(method, path string) bool {
	switch method {
	case http.MethodPost:
		switch path {
		case PasskeySignInBeginPath, PasskeySignInFinishPath, PasskeyRegisterBeginPath, PasskeyRegisterFinishPath:
			return true
		}
	case http.MethodDelete:
		id, ok := strings.CutPrefix(path, PasskeysPath+"/")
		return ok && id != "" && !strings.Contains(id, "/")
	}
	return false
}

// signInRoute reports whether path is a sign-in route, which answers
// without a session: trading a link, and the passkey sign-in ceremony.
func signInRoute(path string) bool {
	return path == SessionPath || path == PasskeySignInBeginPath || path == PasskeySignInFinishPath
}

// grant is the proof one registration needs: made when a sign-in link was
// used or a passkey checked, for the session made with it.
type grant struct {
	person  string
	session [32]byte // the hash of that session
	exp     time.Time
}

// ceremony is a WebAuthn ceremony under way, kept until it finishes or
// expires, and used once.
type ceremony struct {
	register bool
	// Of a registration: whose passkey it makes, the session that began
	// it, the grant it uses, and the user handle.
	person  string
	session [32]byte
	grant   [32]byte
	handle  []byte
	data    webauthn.SessionData
	exp     time.Time
}

// passkeyUser is a person as the WebAuthn library sees them.
type passkeyUser struct {
	handle []byte
	person string
	creds  []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u passkeyUser) WebAuthnName() string                       { return u.person }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.person }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// credential is k as the WebAuthn library checks it. The user was
// verified when it was made.
func (k passkey) credential() webauthn.Credential {
	return webauthn.Credential{ID: k.ID, PublicKey: k.PublicKey,
		Flags:         webauthn.CredentialFlags{UserPresent: true, UserVerified: true, BackupEligible: k.BackupEligible, BackupState: k.BackupState},
		Authenticator: webauthn.Authenticator{SignCount: k.SignCount}}
}

// shortID is the start of a passkey's id, for the log.
func shortID(id []byte) string {
	s := base64.RawURLEncoding.EncodeToString(id)
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// relyingParty makes the WebAuthn relying party of the web listener l.
// Browsers take passkeys only for a domain name, so a web listener named
// by an address has none, and the error says why.
func relyingParty(l *listener) (*webauthn.WebAuthn, error) {
	rp, err := webauthn.New(&webauthn.Config{
		RPID:          l.base.Hostname(),
		RPDisplayName: "hataori",
		RPOrigins:     []string{l.base.String()},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Timeout: RegisterWithin},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("passkeys need the web listener to be named by a domain name (%q): %w", l.base.String(), err)
	}
	return rp, nil
}

// passkeyRoutes adds the passkey routes.
func (s *Server) passkeyRoutes() {
	s.mux.HandleFunc("POST "+PasskeySignInBeginPath, s.passkeySignInBegin)
	s.mux.HandleFunc("POST "+PasskeySignInFinishPath, s.passkeySignInFinish)
	s.mux.HandleFunc("POST "+PasskeyRegisterBeginPath, s.passkeyRegisterBegin)
	s.mux.HandleFunc("POST "+PasskeyRegisterFinishPath, s.passkeyRegisterFinish)
	s.mux.HandleFunc("GET "+PasskeysPath, s.passkeyList)
	s.mux.HandleFunc("DELETE "+PasskeysPath+"/{id}", s.passkeyRemove)
}

// passkeysHere answers and returns false when r may not sign in with or
// register a passkey on the listener it came to.
func (s *Server) passkeysHere(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case listenerOfRequest(r).way != control.WayWeb:
		http.Error(w, "passkeys are for the web listener: here, SSH or this machine is already the proof", http.StatusForbidden)
	case s.keys == nil:
		http.Error(w, "this dashboard keeps no passkeys", http.StatusForbidden)
	case s.rp == nil:
		http.Error(w, s.rpOff, http.StatusForbidden)
	default:
		return true
	}
	return false
}

// readBody decodes r's JSON body into v, and answers when it cannot.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyBody)).Decode(v); err != nil {
		http.Error(w, "not a request this route reads", http.StatusBadRequest)
		return false
	}
	return true
}

// newGrant makes the proof for one registration by the person of session,
// good for RegisterWithin.
func (s *Server) newGrant(person, session string) (*registerToken, error) {
	tok, sum, err := secret()
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, g := range s.grants {
		if !now.Before(g.exp) {
			delete(s.grants, k)
		}
	}
	s.grants[sum] = grant{person: person, session: sha256.Sum256([]byte(session)), exp: now.Add(RegisterWithin)}
	return &registerToken{Token: tok, ExpiresIn: int(RegisterWithin / time.Second)}, nil
}

// registerToken is the proof as the page gets it.
type registerToken struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// grantOf returns the grant tok names when it is still good and was made
// with the session ss.
func (s *Server) grantOf(tok string, ss session) (grant, [32]byte, bool) {
	sum := sha256.Sum256([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[sum]
	return g, sum, ok && tok != "" && g.person == ss.person && g.session == ss.sum && s.now().Before(g.exp)
}

// useGrant takes the grant sum names out, and reports whether it was
// still there and good.
func (s *Server) useGrant(sum [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[sum]
	delete(s.grants, sum)
	return ok && s.now().Before(g.exp)
}

// addCeremony keeps c and returns the id the page finishes it with.
func (s *Server) addCeremony(c ceremony) (string, bool, error) {
	id, sum, err := secret()
	if err != nil {
		return "", false, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, x := range s.ceremonies {
		if !now.Before(x.exp) {
			delete(s.ceremonies, k)
		}
	}
	if len(s.ceremonies) >= maxCeremonies {
		return "", false, nil
	}
	s.ceremonies[sum] = c
	return id, true, nil
}

// takeCeremony takes the ceremony id names out: ok is false when there is
// none, it has expired, or it is not of the kind asked for.
func (s *Server) takeCeremony(id string, register bool) (ceremony, bool) {
	sum := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ceremonies[sum]
	delete(s.ceremonies, sum)
	return c, ok && id != "" && c.register == register && s.now().Before(c.exp)
}

// passkeySignInBegin starts signing in with a passkey: any passkey of this
// relying party, so no name is asked for.
func (s *Server) passkeySignInBegin(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysHere(w, r) {
		return
	}
	opts, data, err := s.rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.Log.Error("dashboard: passkey sign-in", "err", err)
		http.Error(w, "cannot start a passkey sign-in", http.StatusInternalServerError)
		return
	}
	id, ok, err := s.addCeremony(ceremony{data: *data, exp: s.now().Add(ceremonyTTL)})
	if err != nil {
		http.Error(w, "cannot start a passkey sign-in", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.Log.Warn("dashboard: passkey sign-in refused: too many ceremonies under way", "listener", string(control.WayWeb))
		http.Error(w, "too many sign-ins under way: try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, struct {
		Ceremony string                        `json:"ceremony"`
		Options  *protocol.CredentialAssertion `json:"options"`
	}{id, opts})
}

// finishRequest is what the page sends to finish a ceremony.
type finishRequest struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
}

// Why a passkey sign-in opens nothing, as the page is told.
var (
	errUnknownPasskey = errors.New("this passkey is not registered here: it was removed, or made for another dashboard")
	errCounterBack    = errors.New("this passkey's signature counter did not go up, so it may have been copied: it is refused")
	errMayNotSignIn   = errors.New("the person of this passkey may no longer sign in")
)

// passkeySignInFinish checks the passkey's signature and opens a session
// of its person, which records when the passkey was checked. A passkey
// whose signature counter did not go up, or whose person may no longer
// sign in, opens nothing, and that is logged.
func (s *Server) passkeySignInFinish(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysHere(w, r) {
		return
	}
	var req finishRequest
	if !readBody(w, r, &req) {
		return
	}
	c, ok := s.takeCeremony(req.Ceremony, false)
	if !ok {
		http.Error(w, "this passkey sign-in has expired or was finished: start again", http.StatusUnauthorized)
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		http.Error(w, "not a passkey's answer", http.StatusBadRequest)
		return
	}
	now := s.now()
	var who passkey
	err = s.keys.change(func(keys []passkey) ([]passkey, bool, error) {
		i, unknown := -1, false
		_, cred, err := s.rp.ValidatePasskeyLogin(func(rawID, _ []byte) (webauthn.User, error) {
			i = slices.IndexFunc(keys, func(k passkey) bool { return bytes.Equal(k.ID, rawID) })
			if i < 0 {
				unknown = true
				return nil, errUnknownPasskey
			}
			k := keys[i]
			return passkeyUser{handle: k.Handle, person: k.Person, creds: []webauthn.Credential{k.credential()}}, nil
		}, c.data, parsed)
		if unknown {
			return nil, false, errUnknownPasskey
		}
		if i < 0 {
			return nil, false, err // refused before the passkey was looked up
		}
		k := &keys[i]
		if err != nil {
			s.Log.Warn("dashboard: passkey sign-in refused", "person", k.Person, "passkey", shortID(k.ID), "err", err)
			return nil, false, err
		}
		if cred.Authenticator.CloneWarning {
			s.Log.Warn("dashboard: passkey sign-in refused: its signature counter did not go up, so the passkey may have been copied",
				"person", k.Person, "passkey", shortID(k.ID), "counter", parsed.Response.AuthenticatorData.Counter, "last", k.SignCount)
			return nil, false, errCounterBack
		}
		if _, ok := s.For(k.Person); !ok {
			s.Log.Warn("dashboard: passkey sign-in refused: its person may no longer sign in", "person", k.Person, "passkey", shortID(k.ID))
			return nil, false, errMayNotSignIn
		}
		k.SignCount = cred.Authenticator.SignCount
		k.BackupState = cred.Flags.BackupState
		k.Used = &now
		who = *k
		return keys, true, nil
	})
	switch {
	case errors.Is(err, errUnknownPasskey), errors.Is(err, errCounterBack), errors.Is(err, errMayNotSignIn):
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	case err != nil:
		var pe *protocol.Error
		if errors.As(err, &pe) {
			http.Error(w, "the passkey's answer was not accepted", http.StatusUnauthorized)
			return
		}
		s.Log.Error("dashboard: passkey sign-in", "err", err)
		http.Error(w, "cannot sign in with a passkey now", http.StatusInternalServerError)
		return
	}
	tok, err := s.newSession(now.Add(s.web.ttl), who.Person, control.WayWeb, now)
	if err != nil {
		http.Error(w, "cannot make a session", http.StatusInternalServerError)
		return
	}
	reg, err := s.newGrant(who.Person, tok)
	if err != nil {
		http.Error(w, "cannot make a session", http.StatusInternalServerError)
		return
	}
	s.Log.Info("dashboard: signed in with a passkey", "person", who.Person, "passkey", shortID(who.ID))
	writeJSON(w, signedIn{Session: tok, Register: reg})
}

// signedIn is the answer of a sign-in: the session, and on the web
// listener the proof that lets it register a passkey.
type signedIn struct {
	Session  string         `json:"session"`
	Register *registerToken `json:"register,omitempty"`
}

// passkeyRegisterBegin starts registering a passkey for the session's
// person, with the proof the session got.
func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysHere(w, r) {
		return
	}
	ss := sessionOf(r)
	var req struct {
		Register string `json:"register"`
	}
	if !readBody(w, r, &req) {
		return
	}
	g, sum, ok := s.grantOf(req.Register, ss)
	if !ok {
		http.Error(w, "registering a passkey needs a proof made for it: open a new sign-in link, or check a passkey you have, and register within five minutes", http.StatusForbidden)
		return
	}
	keys, err := s.keys.list()
	if err != nil {
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
		return
	}
	u := passkeyUser{person: ss.person}
	var exclude []protocol.CredentialDescriptor
	for _, k := range keys {
		if k.Person == ss.person {
			if u.handle == nil {
				u.handle = k.Handle
			}
			c := k.credential()
			exclude = append(exclude, c.Descriptor())
		}
	}
	if u.handle == nil {
		u.handle = make([]byte, 32)
		if _, err := rand.Read(u.handle); err != nil {
			http.Error(w, "cannot start a registration", http.StatusInternalServerError)
			return
		}
	}
	opts, data, err := s.rp.BeginRegistration(u,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(exclude),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps()))
	if err != nil {
		s.Log.Error("dashboard: passkey registration", "err", err)
		http.Error(w, "cannot start a registration", http.StatusInternalServerError)
		return
	}
	id, ok, err := s.addCeremony(ceremony{register: true, person: ss.person, session: ss.sum, grant: sum, handle: u.handle, data: *data, exp: g.exp})
	if err != nil || !ok {
		http.Error(w, "cannot start a registration now: try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, struct {
		Ceremony string                       `json:"ceremony"`
		Options  *protocol.CredentialCreation `json:"options"`
	}{id, opts})
}

// passkeyRegisterFinish checks the new passkey and keeps it. Its proof is
// used up.
func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.passkeysHere(w, r) {
		return
	}
	ss := sessionOf(r)
	var req finishRequest
	if !readBody(w, r, &req) {
		return
	}
	c, ok := s.takeCeremony(req.Ceremony, true)
	if !ok || c.person != ss.person || c.session != ss.sum {
		http.Error(w, "this registration has expired, was finished, or belongs to another sign-in: start again", http.StatusForbidden)
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		http.Error(w, "not a passkey's answer", http.StatusBadRequest)
		return
	}
	cred, err := s.rp.CreateCredential(passkeyUser{handle: c.handle, person: c.person}, c.data, parsed)
	if err != nil {
		s.Log.Warn("dashboard: passkey registration refused", "person", c.person, "err", err)
		http.Error(w, "the new passkey was not accepted", http.StatusBadRequest)
		return
	}
	// The browser was asked for a discoverable credential; one that says
	// it made another kind would ask for a name at sign-in.
	if cp := parsed.ClientExtensionResults.CredProps; cp != nil && cp.RK != nil && !*cp.RK {
		http.Error(w, "this authenticator made a passkey that cannot be found without a name: use another", http.StatusBadRequest)
		return
	}
	if !s.useGrant(c.grant) {
		http.Error(w, "registering a passkey needs a proof made for it: open a new sign-in link, or check a passkey you have, and register within five minutes", http.StatusForbidden)
		return
	}
	now := s.now()
	k := passkey{ID: cred.ID, Person: c.person, Handle: c.handle, PublicKey: cred.PublicKey, SignCount: cred.Authenticator.SignCount,
		BackupEligible: cred.Flags.BackupEligible, BackupState: cred.Flags.BackupState, Made: now}
	errTaken := errors.New("this passkey is registered already")
	err = s.keys.change(func(keys []passkey) ([]passkey, bool, error) {
		if slices.ContainsFunc(keys, func(x passkey) bool { return bytes.Equal(x.ID, k.ID) }) {
			return nil, false, errTaken
		}
		return append(keys, k), true, nil
	})
	if errors.Is(err, errTaken) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.Log.Error("dashboard: passkey registration", "err", err)
		http.Error(w, "cannot keep the passkey", http.StatusInternalServerError)
		return
	}
	s.Log.Info("dashboard: passkey registered", "person", k.Person, "passkey", shortID(k.ID))
	writeJSON(w, struct {
		Passkey passkeyView `json:"passkey"`
	}{view(k)})
}

// passkeyView is a passkey as the page lists it.
type passkeyView struct {
	ID   string     `json:"id"` // the credential id, base64url
	Made time.Time  `json:"made"`
	Used *time.Time `json:"used,omitempty"`
}

func view(k passkey) passkeyView {
	return passkeyView{ID: base64.RawURLEncoding.EncodeToString(k.ID), Made: k.Made, Used: k.Used}
}

// passkeyList answers with the session's person's passkeys, oldest first.
func (s *Server) passkeyList(w http.ResponseWriter, r *http.Request) {
	ss := sessionOf(r)
	out := []passkeyView{}
	if s.keys != nil {
		keys, err := s.keys.list()
		if err != nil {
			s.Log.Error("dashboard: passkeys", "err", err)
			http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
			return
		}
		for _, k := range keys {
			if k.Person == ss.person {
				out = append(out, view(k))
			}
		}
	}
	writeJSON(w, out)
}

// passkeyRemove removes one of the session's person's passkeys.
func (s *Server) passkeyRemove(w http.ResponseWriter, r *http.Request) {
	ss := sessionOf(r)
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 || s.keys == nil {
		http.Error(w, "no such passkey", http.StatusNotFound)
		return
	}
	var gone passkey
	err = s.keys.change(func(keys []passkey) ([]passkey, bool, error) {
		i := slices.IndexFunc(keys, func(k passkey) bool { return k.Person == ss.person && bytes.Equal(k.ID, id) })
		if i < 0 {
			return nil, false, nil
		}
		gone = keys[i]
		return slices.Delete(keys, i, i+1), true, nil
	})
	if err != nil {
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot remove the passkey", http.StatusInternalServerError)
		return
	}
	if gone.ID == nil {
		http.Error(w, "no such passkey", http.StatusNotFound)
		return
	}
	s.Log.Info("dashboard: passkey removed", "person", gone.Person, "passkey", shortID(gone.ID), "listener", string(listenerOfRequest(r).way))
	w.WriteHeader(http.StatusNoContent)
}
