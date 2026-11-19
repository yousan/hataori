package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// Questions with buttons (design, section 4.4): a question whose options
// fit the surface's choices is posted with one choice per option. Each
// choice's token names the request and the option, and the request is a
// row of the core, so a press after a restart still answers it. The next
// message in the thread answers it too, as in phase 1. Once it is
// answered or withdrawn, the message is edited to lose its choices.

// askToken starts the token of a question's choice: ask:<request>#<option>.
const askToken = "ask:"

// questionPart is the part of an answer's or a withdrawal's row that edits
// the question; part 0 is the answer's line in the other links.
const questionPart = 1

// withButtons reports whether a question with these options is shown with
// one choice per option here.
func (f *FrontEnd) withButtons(choices []string, multi bool) bool {
	l := f.Driver.Limits()
	return len(choices) > 0 && len(choices) <= l.MaxChoices && (!multi || l.MultiSelect)
}

// questionMessage is how a question is shown: with its choices, or as text
// with its options numbered, answered by the next message.
func (f *FrontEnd) questionMessage(request, text string, choices []string, multi bool) surface.Message {
	if !f.withButtons(choices, multi) {
		return surface.Message{Text: question(text, choices)}
	}
	m := surface.Message{Text: strings.TrimSpace(text), Multi: multi}
	for i, c := range choices {
		m.Choices = append(m.Choices, surface.Choice{Label: c, Token: askToken + request + "#" + strconv.Itoa(i)})
	}
	return m
}

// questionDone returns the row that edits the question with request id,
// shown with choices, once e answered or withdrew it: the question stays,
// its choices go, and a line says how it ended. A question shown as text
// is left as it is.
func (f *FrontEnd) questionDone(ctx context.Context, r *reader, e control.Event, conv surface.Conversation, request string) ([]Row, error) {
	if request == "" {
		return nil, nil
	}
	var (
		pos      uint64
		q        requestData
		answered bool
	)
	err := r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		var d requestData
		switch {
		case ev.Kind == "request" && jsonInto(ev.Data, &d) && d.Request == request:
			pos, q = ev.Pos, d
		case ev.Kind == "answer" && jsonInto(ev.Data, &d) && d.Request == request:
			answered = true
		}
	})
	// A withdrawal after the answer leaves the answer shown.
	if err != nil || pos == 0 || q.Approval || !f.withButtons(q.Choices, q.Multi) || answered {
		return nil, err
	}
	end := "(No longer waiting for an answer.)"
	if e.Kind == "answer" {
		var d eventData
		jsonInto(e.Data, &d)
		end = "Answered: " + strings.TrimSpace(d.Text)
	}
	m := surface.Message{Text: strings.TrimSpace(q.Text) + "\n\n" + end}
	return []Row{{Event: e.Pos, Part: questionPart, Conv: conv, Op: Edit, Target: &RowKey{Event: pos, Part: 0}, Message: m}}, nil
}

// requestData is what a request event says, and what an answer or a
// withdrawal names.
type requestData struct {
	Request  string   `json:"request"`
	Approval bool     `json:"approval"`
	Text     string   `json:"text"`
	Choices  []string `json:"choices"`
	Multi    bool     `json:"multi"`
}

// press answers a question with the options s chose, as the person who
// pressed. Whether that person may answer is the core's to decide. A press
// that answers nothing writes one log line and shows nothing: a selection
// has no private reply here.
func (f *FrontEnd) press(s surface.Selection, rest string) {
	ctx := f.context()
	ignored := func(reason string, err error) {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID, "err", err)
	}
	i := strings.LastIndex(rest, "#")
	if i < 0 {
		ignored("no button of the front end's", nil)
		return
	}
	request := control.RequestID(rest[:i])
	tokens := s.Tokens
	if len(tokens) == 0 {
		tokens = []string{s.Token}
	}
	t, ok, err := f.threadHere(ctx, s.Ref.Conv)
	if err != nil || !ok {
		ignored("no hataori thread", err)
		return
	}
	// The options as the core holds them, while the question is open.
	d, err := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{t.Scope}, May: control.OpRead}).Thread(ctx, t.ID, 0, 0)
	if err != nil {
		ignored("the thread cannot be read", err)
		return
	}
	var open *control.Request
	for k := range d.Requests {
		if d.Requests[k].ID == request {
			open = &d.Requests[k]
		}
	}
	if open == nil {
		ignored("the question is no longer open", nil)
		return
	}
	var chosen []string
	for _, tok := range tokens {
		r, n, ok := strings.Cut(strings.TrimPrefix(tok, askToken), "#")
		idx, err := strconv.Atoi(n)
		if !ok || r != string(request) || err != nil || idx < 0 || idx >= len(open.Choices) {
			ignored("an option the question does not have", nil)
			return
		}
		chosen = append(chosen, open.Choices[idx].Token)
	}
	conv := s.Ref.Conv
	api := f.API(control.Caller{From: f.from(s.Author), Scopes: f.scopesIn(conv.Channel), May: control.OpRead | control.OpAnswer})
	err = api.Answer(ctx, request, control.Answer{Text: strings.Join(chosen, ", "), Choices: chosen,
		Link: &control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}})
	switch {
	case errors.Is(err, control.ErrForbidden):
		// The core logged it.
	case err != nil:
		ignored("the answer was not taken", err)
	}
}

// jsonInto decodes b into v and reports whether it could.
func jsonInto(b json.RawMessage, v any) bool { return len(b) > 0 && json.Unmarshal(b, v) == nil }
