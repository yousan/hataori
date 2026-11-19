package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// Typing into a tmux pane on the host that hataori does not own (design,
// section 7.2, "Typing into a tmux pane on the host"). A tab that operates
// a pane opens a WebSocket of its own, which starts the operation
// (control.API.OperateHost) only when the way the person came in allows it,
// carries the keys in order, and ends the operation when it closes:
//
//	POST /api/operate/check  {"pane"} → {"needs": "nothing"}, or {"needs": "passkey", "ceremony", "options"}
//	GET  /api/operate        a WebSocket; its first message is {"session", "pane"[, "ceremony", "credential"]}
//
// On the loopback listener an operation starts at once. On the web
// listener it starts only after a passkey check with user verification
// made for this start, the ceremony that the check route began for this
// session and this pane; a person with no passkey needs a session signed in
// less than OperateSignInWithin ago. Otherwise the start is refused with a
// reason the page shows.
//
// The WebSocket is the only route that accepts an Upgrade. A browser cannot
// send a header with a WebSocket, so its handshake carries no session: the
// session comes in its first message, and nothing else is accepted on it
// before. Its handshake needs this listener's exact Origin, as a request
// that changes something does, and is refused while host input is off.
//
// After the start the server sends {"started": {"idle": <seconds>}}. The
// page sends {"keys": [HostKey...]}, carried out in order, and {"end":
// true}; a batch that is refused gets {"error": "..."} and the operation
// goes on. When the operation ends, for whatever reason, the server sends
// {"ended": "<reason>"} and closes the WebSocket with the reason. A refusal
// of the start sends {"refused": "<reason>"} and closes with it. Closing
// the WebSocket ends the operation at once. Nothing the page types is
// logged.
//
// The page asks first whether the person may operate at all, and shows the
// switch only then:
//
//	GET  /api/operate/may    {"may": true|false}
const (
	OperatePath      = "/api/operate"
	OperateCheckPath = "/api/operate/check"
	OperateMayPath   = "/api/operate/may"
)

// OperateSignInWithin is how recent a web session's sign-in must be for a
// person with no passkey to start an operation there.
const OperateSignInWithin = 5 * time.Minute

const (
	// firstMessageTimeout is how long the WebSocket waits for its first
	// message.
	firstMessageTimeout = 10 * time.Second
	// maxOperateMessage is the largest message the page may send.
	maxOperateMessage = 1 << 20
	// maxQueuedBatches is how many batches may wait behind the one being
	// sent; one more is refused.
	maxQueuedBatches = 64
	// defaultOperatedScreenInterval is the wait between two reads of the
	// screen of a pane being operated: short enough that, with the read
	// itself, it is read at least five times a second.
	defaultOperatedScreenInterval = 150 * time.Millisecond
)

// operateRoutes adds the operate routes.
func (s *Server) operateRoutes() {
	s.mux.HandleFunc("GET "+OperateMayPath, s.operateMay)
	s.mux.HandleFunc("POST "+OperateCheckPath, s.operateCheck)
	s.mux.HandleFunc("GET "+OperatePath, s.operate)
}

// operateMay says whether the session's person may type into a host tmux
// pane: host input is on and the daemon gives that person's Caller
// OpHostInput. It changes nothing; the core still decides each start.
func (s *Server) operateMay(w http.ResponseWriter, r *http.Request) {
	may := s.hostInput && s.mayOperate != nil && s.mayOperate(sessionOf(r).person)
	writeJSON(w, struct {
		May bool `json:"may"`
	}{may})
}

// errInputOff is the answer while the instance's host input is off.
var errInputOff = errors.New(`typing into host panes is off: the config's "host_input" turns it on`)

// operateCheck says what the session must show to start operating a pane
// on the listener it came to: on the loopback listener nothing; on the web
// listener a passkey check made for this start, which it begins, or, for a
// person with no passkey, a sign-in less than OperateSignInWithin old.
func (s *Server) operateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.hostInput {
		http.Error(w, errInputOff.Error(), http.StatusForbidden)
		return
	}
	var req struct {
		Pane string `json:"pane"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Pane == "" {
		http.Error(w, "pane is required", http.StatusBadRequest)
		return
	}
	type nothing struct {
		Needs string `json:"needs"`
	}
	l := listenerOfRequest(r)
	if l.way != control.WayWeb {
		writeJSON(w, nothing{"nothing"})
		return
	}
	ss := sessionOf(r)
	creds, err := s.passkeysOf(ss.person)
	if err != nil {
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
		return
	}
	if len(creds.creds) == 0 {
		if reason := s.notRecent(ss.claims, l); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
		writeJSON(w, nothing{"nothing"})
		return
	}
	opts, data, err := s.rp.BeginLogin(creds, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.Log.Error("dashboard: passkey check to operate", "err", err)
		http.Error(w, "cannot start a passkey check", http.StatusInternalServerError)
		return
	}
	id, ok, err := s.addCeremony(ceremony{person: ss.person, session: ss.sum, operate: req.Pane, data: *data, exp: s.now().Add(ceremonyTTL)})
	if err != nil || !ok {
		http.Error(w, "cannot start a passkey check now: try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, struct {
		Needs    string                        `json:"needs"`
		Ceremony string                        `json:"ceremony"`
		Options  *protocol.CredentialAssertion `json:"options"`
	}{"passkey", id, opts})
}

// passkeysOf is person as the WebAuthn library sees them, with their
// passkeys; none when the dashboard keeps none.
func (s *Server) passkeysOf(person string) (passkeyUser, error) {
	if s.keys == nil || s.rp == nil {
		return passkeyUser{person: person}, nil
	}
	keys, err := s.keys.list()
	if err != nil {
		return passkeyUser{}, err
	}
	return userOf(person, keys), nil
}

// notRecent is why a session of a person with no passkey may not start an
// operation on the web listener l, or "" when it may: it must be a
// session of a sign-in link, signed in less than OperateSignInWithin ago,
// after this server started and after its person last removed a passkey.
// So a session cannot remove its person's passkeys and then do without a
// check, nor can one that a passkey made, whose person had a passkey. A
// session does not record its sign-in, only its expiry, which is the
// sign-in and l's lifetime.
func (s *Server) notRecent(c claims, l *listener) string {
	const again = "typing into a pane from the web needs a passkey check, or a sign-in less than five minutes old: sign in again with a new link"
	signedIn := c.exp.Add(-l.ttl)
	s.mu.Lock()
	floor := s.started
	if r, ok := s.removed[c.person]; ok && r.After(floor) {
		floor = r
	}
	s.mu.Unlock()
	// A sign-in is known to the second: one in the same second as the
	// removal or the start counts as before it.
	if !c.checked.IsZero() || !signedIn.After(floor.Truncate(time.Second)) || s.now().Sub(signedIn) >= OperateSignInWithin {
		return again
	}
	return ""
}

// operateFirst is the WebSocket's first message.
type operateFirst struct {
	Session    string          `json:"session"`
	Pane       string          `json:"pane"`
	Ceremony   string          `json:"ceremony,omitempty"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// operateIn is every later message of the page.
type operateIn struct {
	Keys []control.HostKey `json:"keys,omitempty"`
	End  bool              `json:"end,omitempty"`
}

// operateOut is a message to the page.
type operateOut struct {
	Started *struct {
		Idle int64 `json:"idle"` // seconds
	} `json:"started,omitempty"`
	Error   string `json:"error,omitempty"`
	Ended   string `json:"ended,omitempty"`
	Refused string `json:"refused,omitempty"`
}

// The handshake's own checks are made before it, in handler: the Host,
// and an Origin that is exactly the listener's.
var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin:      func(*http.Request) bool { return true }, // checked exactly in handler
}

// wsConn is a WebSocket with one writer at a time.
type wsConn struct {
	*websocket.Conn
	mu      sync.Mutex
	timeout time.Duration
}

func (c *wsConn) send(v operateOut) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.WriteJSON(v)
}

// close sends a close frame with code and reason, and closes the
// connection.
func (c *wsConn) close(code int, reason string) {
	if len(reason) > 120 { // a close frame's reason is at most 123 bytes, and UTF-8
		reason = strings.ToValidUTF8(reason[:120], "")
	}
	c.mu.Lock()
	c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	c.mu.Unlock()
	c.Close()
}

// refuse tells the page why its start is refused and closes.
func (c *wsConn) refuse(reason string) {
	c.send(operateOut{Refused: reason})
	c.close(websocket.ClosePolicyViolation, reason)
}

// operate serves the WebSocket of one operation.
func (s *Server) operate(w http.ResponseWriter, r *http.Request) {
	l := listenerOfRequest(r)
	if !s.hostInput {
		s.refuse(w, r, l, http.StatusForbidden, errInputOff.Error())
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader answered
	}
	c := &wsConn{Conn: ws, timeout: s.writeTimeout}
	defer c.Close()
	// Before the session is known, no more than a passkey's answer.
	c.SetReadLimit(maxPasskeyBody)

	// The session first, and nothing else before it.
	c.SetReadDeadline(time.Now().Add(firstMessageTimeout))
	var first operateFirst
	kind, b, err := c.ReadMessage()
	if err != nil {
		return
	}
	if kind != websocket.TextMessage || json.Unmarshal(b, &first) != nil {
		c.refuse("the first message must be the session")
		return
	}
	c.SetReadDeadline(time.Time{})
	claims, ok := s.valid(first.Session, l)
	var a control.API
	if ok {
		a, ok = s.For(claims.person)
	}
	if !ok || a == nil {
		c.refuse("not signed in")
		return
	}
	if first.Pane == "" {
		c.refuse("pane is required")
		return
	}
	if l.way == control.WayWeb {
		if reason := s.mayStartOnWeb(claims, sha256.Sum256([]byte(first.Session)), l, first); reason != "" {
			s.Log.Warn("dashboard: an operation refused on the web listener", "person", claims.person, "pane", first.Pane, "reason", reason)
			c.refuse(reason)
			return
		}
	}

	// The operation lasts at most as long as the session, and ends when
	// the WebSocket closes.
	ctx, cancel := context.WithDeadline(r.Context(), claims.exp)
	defer cancel()
	op, err := a.OperateHost(ctx, first.Pane)
	if err != nil {
		c.refuse(operateRefusal(err))
		return
	}
	s.operating(first.Pane, 1)
	defer s.operating(first.Pane, -1)
	c.SetReadLimit(maxOperateMessage)
	// A page that stops answering pings has gone, as one that closes has:
	// a browser answers each ping.
	alive := func() { c.SetReadDeadline(time.Now().Add(2*s.keepAlive + s.writeTimeout)) }
	alive()
	c.SetPongHandler(func(string) error { alive(); return nil })
	if c.send(operateOut{Started: &struct {
		Idle int64 `json:"idle"`
	}{int64(op.Idle() / time.Second)}}) != nil {
		cancel()
		<-op.Done()
		return
	}

	// Batches are carried out in order by one goroutine, while this one
	// keeps reading, so that an end or the page going away takes effect
	// during a batch.
	// An end ends the operation after the batches the page sent before it,
	// which the batch goroutine sends first; a WebSocket that closes ends it
	// at once. The reader never waits for the batches.
	batches := make(chan []control.HostKey, maxQueuedBatches)
	endAsked := make(chan struct{})
	go func() {
		send := func(keys []control.HostKey) {
			if err := op.Send(ctx, keys); err != nil && !errors.Is(err, control.ErrEnded) && ctx.Err() == nil {
				c.send(operateOut{Error: err.Error()})
			}
		}
		for {
			select {
			case keys, ok := <-batches:
				if !ok {
					return
				}
				send(keys)
			case <-endAsked:
				for drained := false; !drained; {
					select {
					case keys, ok := <-batches:
						if !ok {
							op.End()
							return
						}
						send(keys)
					default:
						drained = true
					}
				}
				op.End()
				for range batches { // nothing is sent after the end
				}
				return
			}
		}
	}()
	ended := false // the page asked for the end: later keys are dropped
	go func() {
		defer close(batches)
		defer cancel() // the page went away: the operation ends
		for {
			kind, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m operateIn
			if kind != websocket.TextMessage || json.Unmarshal(b, &m) != nil {
				// The message of a decoding error may quote the text.
				c.send(operateOut{Error: "not a message of an operation: nothing was sent"})
				continue
			}
			if ended {
				continue
			}
			if m.End {
				ended = true
				close(endAsked)
				continue
			}
			if len(m.Keys) == 0 {
				continue
			}
			select {
			case batches <- m.Keys:
			default:
				c.send(operateOut{Error: "too many batches wait to be sent: this one was not"})
			}
		}
	}()
	pings := time.NewTicker(s.keepAlive)
	defer pings.Stop()
	for {
		select {
		case <-op.Done():
			c.send(operateOut{Ended: op.Reason()})
			c.close(websocket.CloseNormalClosure, op.Reason())
			return
		case <-pings.C:
			c.mu.Lock()
			err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.writeTimeout))
			c.mu.Unlock()
			if err != nil {
				cancel()
			}
		}
	}
}

// operateRefusal is what the page is told when OperateHost refuses.
func operateRefusal(err error) string {
	switch {
	case errors.Is(err, control.ErrForbidden):
		return "you may not type into this pane: only the owner may, into a pane that shows no session of hataori's"
	case errors.Is(err, core.ErrNotFound):
		return "this pane is not there any more"
	case errors.Is(err, core.ErrBusy):
		return strings.TrimPrefix(err.Error(), "core: OperateHost: ")
	}
	return "the operation cannot start now"
}

// mayStartOnWeb is why a start on the web listener is refused, or "" when
// it may go on: a passkey check made for it, or for a person with no
// passkey a recent sign-in.
func (s *Server) mayStartOnWeb(c claims, sum [32]byte, l *listener, first operateFirst) string {
	u, err := s.passkeysOf(c.person)
	if err != nil {
		s.Log.Error("dashboard: passkeys", "err", err)
		return "the passkeys cannot be read now"
	}
	if len(u.creds) == 0 {
		return s.notRecent(c, l)
	}
	if first.Ceremony == "" || len(first.Credential) == 0 {
		return "typing into a pane from the web needs a passkey check made for it: check your passkey first"
	}
	cer, ok := s.takeCeremony(first.Ceremony, false)
	if !ok || cer.operate == "" || cer.operate != first.Pane || cer.person != c.person || cer.session != sum {
		return "this passkey check has expired, was used, or was made for another start: check your passkey again"
	}
	if err := s.checkPasskey(cer, first.Credential); err != nil {
		return err.Error()
	}
	return ""
}

// checkPasskey checks a passkey's answer to the ceremony cer of its person,
// with user verification, and records its use. A counter that did not go
// up is refused and logged.
func (s *Server) checkPasskey(cer ceremony, raw json.RawMessage) error {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return errors.New("not a passkey's answer")
	}
	now := s.now()
	err = s.keys.change(func(keys []passkey) ([]passkey, bool, error) {
		i := slices.IndexFunc(keys, func(k passkey) bool { return bytes.Equal(k.ID, parsed.RawID) })
		if i < 0 {
			return nil, false, errUnknownPasskey
		}
		k := &keys[i]
		if k.Person != cer.person {
			return nil, false, errNotYours
		}
		cred, err := s.rp.ValidateLogin(userOf(cer.person, keys), cer.data, parsed)
		if err != nil {
			s.Log.Warn("dashboard: a passkey check to operate refused", "person", k.Person, "passkey", shortID(k.ID), "err", err)
			return nil, false, errors.New("the passkey's answer was not accepted")
		}
		if cred.Authenticator.CloneWarning {
			s.Log.Warn("dashboard: a passkey check to operate refused: its signature counter did not go up, so the passkey may have been copied",
				"person", k.Person, "passkey", shortID(k.ID))
			return nil, false, errCounterBack
		}
		k.SignCount = cred.Authenticator.SignCount
		k.BackupState = cred.Flags.BackupState
		k.Used = &now
		return keys, true, nil
	})
	return err
}

// operating counts the operations of pane open through this server, and
// wakes the screen streams, so that one waiting its slow interval reads at
// the fast one at once.
func (s *Server) operating(pane string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operated == nil {
		s.operated = map[string]int{}
	}
	if s.operated[pane] += n; s.operated[pane] <= 0 {
		delete(s.operated, pane)
	}
	if s.operatedChanged != nil {
		close(s.operatedChanged)
	}
	s.operatedChanged = make(chan struct{})
}

// operatedChange is closed the next time an operation starts or ends
// through this server.
func (s *Server) operatedChange() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operatedChanged == nil {
		s.operatedChanged = make(chan struct{})
	}
	return s.operatedChanged
}

// isOperated reports whether pane is being operated through this server.
func (s *Server) isOperated(pane string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operated[pane] > 0
}
