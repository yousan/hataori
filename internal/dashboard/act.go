package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	hsession "github.com/yousan/hataori/internal/session"
)

// Giving a thread input from its tab (design, section 7.2): sending,
// answering a question or an approval, stopping the running turn, and
// choosing for a start that files of the repository blocked, each one call
// of the control API as the session's person, whose Caller the daemon gives
// OpSend, OpAnswer, OpStop, OpStart, OpTrust and OpClose for the owner only. The core
// checks each call as it checks chat's, and its events say the call came
// through the dashboard (Caller.Via).
//
//	GET  /api/act/may     {"may": true|false}
//	POST /api/act/check   {"needs": "nothing"}, or {"needs": "passkey", "ceremony", "options"}
//	POST /api/act/verify  {"ceremony", "credential"} → {"until": <seconds>}
//	POST /api/send        {"thread", "key", "text"}
//	POST /api/answer      {"request", "text" | "choices" | "allow"}
//	POST /api/stop        {"thread", "turn"}
//	POST /api/start/choose {"thread", "session", "choice", "files"}
//
// On the loopback listener a signed-in session acts at once. On the web
// listener each send, answer, stop and choice needs a passkey check with user
// verification that the session made through act/check and act/verify
// less than ActCheckWithin ago, and before which the person removed no
// passkey; a person with no passkey needs instead a
// session of a sign-in link less than OperateSignInWithin old, made after
// the server started and after they last removed a passkey, as for
// operating a pane. Without it a call answers 428 with the reason, and the
// page asks for the check, then makes the call again. The checks are kept
// in memory, so a restart asks for them again.
//
// A refusal answers with the reason and a status: 403 when the caller may
// not, 404 for no such thread or request, 409 for a question that is not
// open, a turn that is not running, a start that is not blocked any more,
// or a thread that is closed.
const (
	ActMayPath    = "/api/act/may"
	ActCheckPath  = "/api/act/check"
	ActVerifyPath = "/api/act/verify"
	SendPath      = "/api/send"
	AnswerPath    = "/api/answer"
	StopPath      = "/api/stop"
	ChoosePath    = "/api/start/choose"
)

// ActCheckWithin is how long a passkey check lets a session of the web
// listener act.
const ActCheckWithin = 5 * time.Minute

// actTimeout bounds how long a send, an answer or a stop may take.
const actTimeout = 30 * time.Second

// maxSendText is the largest text a send or an answer may carry.
const maxSendText = 32 << 10

// actRoutes adds the routes that give a thread input.
func (s *Server) actRoutes() {
	s.mux.HandleFunc("GET "+ActMayPath, s.actMay)
	s.mux.HandleFunc("POST "+ActCheckPath, s.actCheck)
	s.mux.HandleFunc("POST "+ActVerifyPath, s.actVerify)
	s.mux.HandleFunc("POST "+SendPath, s.send)
	s.mux.HandleFunc("POST "+AnswerPath, s.answer)
	s.mux.HandleFunc("POST "+StopPath, s.stop)
	s.mux.HandleFunc("POST "+ChoosePath, s.choose)
}

// actPaths are the routes that take a POST from the page to act.
var actPaths = []string{ActCheckPath, ActVerifyPath, SendPath, AnswerPath, StopPath, ChoosePath}

// mayAct reports whether the session's person may give threads input from
// the dashboard: the daemon answers from the rule that gives their Caller
// OpSend. It changes nothing; the core still decides each call.
func (s *Server) mayActAs(person string) bool { return s.mayAct != nil && s.mayAct(person) }

func (s *Server) actMay(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, struct {
		May bool `json:"may"`
	}{s.mayActAs(sessionOf(r).person)})
}

// actRefusal is why a session of the web listener may not act now, or ""
// when it may: a passkey check it made less than ActCheckWithin ago, or,
// for a person with no passkey, a recent sign-in with a link (notRecent).
// passkey says whether the person has a passkey, so that the page knows
// which check to ask for.
func (s *Server) actRefusal(ss session, l *listener) (reason string, passkey bool, err error) {
	if l.way != control.WayWeb {
		return "", false, nil
	}
	// A check counts for ActCheckWithin after it began, and not once the
	// person has removed a passkey since: a session cannot keep acting on
	// the strength of a passkey that is gone.
	s.mu.Lock()
	at, ok := s.acted[ss.sum]
	removed, gone := s.removed[ss.person]
	s.mu.Unlock()
	if ok && s.now().Sub(at) < ActCheckWithin && (!gone || at.After(removed)) {
		return "", true, nil
	}
	u, err := s.passkeysOf(ss.person)
	if err != nil {
		return "", false, err
	}
	if len(u.creds) > 0 {
		return "acting from the web needs a passkey check less than five minutes old: check your passkey", true, nil
	}
	if s.notRecent(ss.claims, l) != "" {
		return "acting from the web needs a passkey check, or a sign-in less than five minutes old: sign in again with a new link", false, nil
	}
	return "", false, nil
}

// actCheck says what the session must show before it may act on the
// listener it came to: nothing, or a passkey check, which it begins.
func (s *Server) actCheck(w http.ResponseWriter, r *http.Request) {
	l, ss := listenerOfRequest(r), sessionOf(r)
	type nothing struct {
		Needs string `json:"needs"`
	}
	reason, passkey, err := s.actRefusal(ss, l)
	switch {
	case err != nil:
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
		return
	case reason == "":
		writeJSON(w, nothing{"nothing"})
		return
	case !passkey:
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	creds, err := s.passkeysOf(ss.person)
	if err != nil {
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
		return
	}
	opts, data, err := s.rp.BeginLogin(creds, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.Log.Error("dashboard: passkey check to act", "err", err)
		http.Error(w, "cannot start a passkey check", http.StatusInternalServerError)
		return
	}
	id, ok, err := s.addCeremony(ceremony{person: ss.person, session: ss.sum, act: true, data: *data, exp: s.now().Add(ceremonyTTL)})
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

// actVerify takes the answer of the passkey check that act/check began for
// this session, and lets the session act for ActCheckWithin.
func (s *Server) actVerify(w http.ResponseWriter, r *http.Request) {
	l, ss := listenerOfRequest(r), sessionOf(r)
	var req struct {
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if l.way != control.WayWeb || s.rp == nil || s.keys == nil {
		http.Error(w, "no passkey check is needed on this listener", http.StatusBadRequest)
		return
	}
	cer, ok := s.takeCeremony(req.Ceremony, false)
	if !ok || !cer.act || cer.person != ss.person || cer.session != ss.sum {
		http.Error(w, "this passkey check has expired, was used, or was made for another session: check your passkey again", http.StatusForbidden)
		return
	}
	// The check is recorded as made when it began: a passkey removed while
	// it was being checked leaves it counting for nothing.
	began := s.now()
	if err := s.checkPasskey(cer, req.Credential); err != nil {
		s.Log.Warn("dashboard: a passkey check to act refused", "person", ss.person, "err", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	now := s.now()
	s.mu.Lock()
	for k, at := range s.acted {
		if now.Sub(at) >= ActCheckWithin {
			delete(s.acted, k)
		}
	}
	s.acted[ss.sum] = began
	s.mu.Unlock()
	writeJSON(w, struct {
		Until int64 `json:"until"`
	}{int64(ActCheckWithin / time.Second)})
}

// acting answers and returns false when the request may not act: the web
// listener's check is missing (428), or it cannot be told.
func (s *Server) acting(w http.ResponseWriter, r *http.Request) bool {
	reason, _, err := s.actRefusal(sessionOf(r), listenerOfRequest(r))
	switch {
	case err != nil:
		s.Log.Error("dashboard: passkeys", "err", err)
		http.Error(w, "cannot read the passkeys", http.StatusInternalServerError)
		return false
	case reason != "":
		http.Error(w, reason, http.StatusPreconditionRequired)
		return false
	}
	return true
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread control.ThreadID `json:"thread"`
		Key    string           `json:"key"`
		Text   string           `json:"text"`
	}
	if !readBody(w, r, &req) || !s.acting(w, r) {
		return
	}
	switch {
	case req.Thread <= 0:
		http.Error(w, "thread is required: the thread's number", http.StatusBadRequest)
		return
	case req.Key == "" || len(req.Key) > 128:
		http.Error(w, "key is required: one of the page's own for this message, so that sending it again sends it once", http.StatusBadRequest)
		return
	case strings.TrimSpace(req.Text) == "":
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	case len(req.Text) > maxSendText:
		http.Error(w, "the text is too long", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actTimeout)
	defer cancel()
	// A message while a turn runs is delivered as a chat message is.
	s.actAnswer(w, api(r).Send(ctx, req.Thread, "dashboard:"+req.Key, control.Input{Text: req.Text}, control.Deliver))
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Request control.RequestID `json:"request"`
		Text    string            `json:"text"`
		Choices []string          `json:"choices"`
		Allow   bool              `json:"allow"`
	}
	if !readBody(w, r, &req) || !s.acting(w, r) {
		return
	}
	switch {
	case req.Request == "":
		http.Error(w, "request is required: the question's or the approval's id", http.StatusBadRequest)
		return
	case len(req.Text) > maxSendText:
		http.Error(w, "the text is too long", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actTimeout)
	defer cancel()
	s.actAnswer(w, api(r).Answer(ctx, req.Request, control.Answer{Text: req.Text, Choices: req.Choices, Allow: req.Allow}))
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread control.ThreadID `json:"thread"`
		Turn   int64            `json:"turn"`
	}
	if !readBody(w, r, &req) || !s.acting(w, r) {
		return
	}
	if req.Thread <= 0 {
		http.Error(w, "thread is required: the thread's number", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actTimeout)
	defer cancel()
	s.actAnswer(w, api(r).Stop(ctx, req.Thread, req.Turn))
}

// choose carries out a choice for a start that files blocked: what the
// chat's button of that choice does, through the same call. session and
// files are the blocked session's, as the page showed them.
func (s *Server) choose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread  control.ThreadID    `json:"thread"`
		Session int64               `json:"session"`
		Choice  control.StartChoice `json:"choice"`
		Files   []string            `json:"files"`
	}
	if !readBody(w, r, &req) || !s.acting(w, r) {
		return
	}
	switch {
	case req.Thread <= 0:
		http.Error(w, "thread is required: the thread's number", http.StatusBadRequest)
		return
	case req.Session <= 0:
		http.Error(w, "session is required: the blocked session", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actTimeout)
	defer cancel()
	a := api(r)
	// A close names no session to the core: as the chat's button does, make
	// sure that the thread still waits for this choice.
	t, err := a.Thread(ctx, req.Thread, 0, 0)
	if err != nil {
		s.actAnswer(w, err)
		return
	}
	if f := t.FailedStart; t.State != control.Failed || f == nil || f.Session != req.Session || len(f.Trust) == 0 {
		http.Error(w, notBlocked, http.StatusConflict)
		return
	}
	switch req.Choice {
	case control.StartTrusted:
		err = a.Trust(ctx, req.Thread, req.Session, req.Files)
	case control.StartWithoutSettings:
		err = a.StartWithoutSettings(ctx, req.Thread, req.Session)
	case control.StartClosed:
		err = a.CloseThread(ctx, req.Thread)
	default:
		http.Error(w, "choice is required: trusted, without project settings or closed", http.StatusBadRequest)
		return
	}
	if errors.Is(err, hsession.ErrRefused) {
		http.Error(w, notBlocked, http.StatusConflict)
		return
	}
	s.actAnswer(w, err)
}

// notBlocked is the answer to a choice that a start does not wait for.
const notBlocked = "the thread no longer waits for this choice: nothing was changed"

// actAnswer answers a send, an answer or a stop: 204, or the refusal with its
// reason and its status.
func (s *Server) actAnswer(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	code, reason := actStatus(err)
	if code == http.StatusInternalServerError {
		s.Log.Warn("dashboard: a call to act failed", "err", err)
	}
	http.Error(w, reason, code)
}

// actStatus is the status and the reason the page is told for err.
func actStatus(err error) (int, string) {
	var stopped *control.Stopped
	switch {
	case errors.Is(err, control.ErrForbidden):
		return http.StatusForbidden, "you may not do this in this thread"
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, "no such thread or question"
	case errors.Is(err, core.ErrNotOpen):
		return http.StatusConflict, "the question is no longer open: it was answered, withdrawn, or its turn ended"
	case errors.Is(err, control.ErrClosed):
		return http.StatusConflict, "the thread is closed"
	case errors.Is(err, hsession.ErrRefused):
		return http.StatusConflict, "that turn is no longer running: nothing was stopped"
	case errors.Is(err, control.ErrNotInterrupted):
		return http.StatusConflict, "the agent's window did not take the interrupt: the turn goes on"
	case errors.Is(err, core.ErrBusy):
		return http.StatusConflict, "a turn is running"
	case errors.As(err, &stopped):
		return http.StatusConflict, "a plugin stopped it: " + stopped.Reason
	}
	return http.StatusInternalServerError, "it could not be done now"
}
