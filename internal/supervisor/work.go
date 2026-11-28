package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// WorkPath opts one scope into durable work context using its existing files.
// It is model-owned data, never an authority or delivery receipt.
const WorkPath = "notes/work.json"

const ToolWork = "work_record"
const workMax = 8 << 10

// WorkRecord is deliberately one item, with user-defined completion prose.
// Evidence and Receipt retain claims/references; neither is independently
// verified by storing it. ReportThread is restricted to the existing own thread.
type WorkRecord struct {
	Schema       int              `json:"schema"`
	Objective    string           `json:"objective"`
	Completion   string           `json:"completion"`
	SourceThread control.ThreadID `json:"source_thread"`
	ReportThread control.ThreadID `json:"report_thread"`
	Worker       control.ThreadID `json:"worker,omitempty"`
	State        string           `json:"state"`
	Wait         string           `json:"wait,omitempty"`
	Next         string           `json:"next"`
	Evidence     string           `json:"evidence,omitempty"`
	Receipt      string           `json:"receipt,omitempty"`
}

var workTool = tool{ToolWork,
	"Read or replace one optional durable work record in notes/work.json. Read at the start of a turn; writes require the exact version returned by read (omit only to create). " +
		"The user chooses objective, completion condition and reporting style. Source is a same-scope thread reference, not requester identity; evidence and receipt text are unverified claims/references. " +
		"The report thread must be this scope's current own supervisor thread. Its permitted wakes receive the record and their ordinary final answers report there; this tool sends nothing. " +
		"Set a reminder separately when needed; a record does not schedule a wake, authorize an action or prove delivery/completion. Preserve a refused reminder as a wait. " +
		"Use state active, waiting, complete or cancelled; complete is your assessment under the user's condition. Do not replace a different unfinished objective. No file is removed.",
	object(map[string]any{
		"op":      map[string]any{"type": "string", "enum": []string{"read", "write"}},
		"version": map[string]any{"type": "string"},
		"record": object(map[string]any{
			"schema":    map[string]any{"type": "integer", "enum": []int{1}},
			"objective": map[string]any{"type": "string"}, "completion": map[string]any{"type": "string"},
			"source_thread": map[string]any{"type": "integer"}, "report_thread": map[string]any{"type": "integer"},
			"worker": map[string]any{"type": "integer"}, "state": map[string]any{"type": "string", "enum": []string{"active", "waiting", "complete", "cancelled"}},
			"wait": map[string]any{"type": "string"}, "next": map[string]any{"type": "string"},
			"evidence": map[string]any{"type": "string"}, "receipt": map[string]any{"type": "string"},
		}, "schema", "objective", "completion", "source_thread", "report_thread", "state", "next"),
	}, "op")}

func decodeWork(b []byte, v any) error {
	if len(b) > workMax || !utf8.Valid(b) || !json.Valid(b) || halfRune(b) {
		return errors.New("work record must be valid JSON within 8 KB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("work record: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("work record has trailing data")
	}
	return nil
}

func (r WorkRecord) valid() error {
	if r.Schema != 1 || strings.TrimSpace(r.Objective) == "" || strings.TrimSpace(r.Completion) == "" ||
		strings.TrimSpace(r.Next) == "" || r.SourceThread <= 0 || r.ReportThread <= 0 || r.Worker < 0 {
		return errors.New("work record needs schema 1, objective, completion, source/report threads and next action")
	}
	switch r.State {
	case "active", "complete", "cancelled":
	case "waiting":
		if strings.TrimSpace(r.Wait) == "" {
			return errors.New("waiting work needs a wait reason")
		}
	default:
		return errors.New("unknown work state")
	}
	return nil
}

func workRefs(ctx context.Context, api control.API, scope string, r WorkRecord) error {
	for _, id := range []control.ThreadID{r.SourceThread, r.ReportThread, r.Worker} {
		if id == 0 {
			continue
		}
		ts, err := api.Threads(ctx, control.Filter{Scope: scope, Thread: id})
		if err != nil {
			return err
		}
		if len(ts) != 1 || ts[0].ID != id || ts[0].Scope != scope {
			return errors.New("work thread is not in this scope")
		}
		if id == r.ReportThread && (ts[0].Kind != control.KindSupervisor || ts[0].State == control.Closed || ts[0].State == control.Removed) {
			return errors.New("report destination must be this scope's current own supervisor thread")
		}
		if id == r.Worker && ts[0].Kind != control.KindOrdinary {
			return errors.New("work owner must be an ordinary worker")
		}
	}
	return nil
}

func (m *MCP) work(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Op      string      `json:"op"`
		Version string      `json:"version"`
		Record  *WorkRecord `json:"record"`
	}
	if err := decodeWork(raw, &a); err != nil {
		return "", err
	}
	api, err := m.connect()
	if err != nil {
		return "", err
	}
	req := control.FilesRequest{Op: control.FilesRead, Path: WorkPath}
	switch a.Op {
	case "read":
		if a.Record != nil || a.Version != "" {
			return "", errors.New("read takes no record or version")
		}
	case "write":
		if a.Record == nil {
			return "", errors.New("write needs a record")
		}
		if err := a.Record.valid(); err != nil {
			return "", err
		}
		if err := workRefs(ctx, api, m.Scope, *a.Record); err != nil {
			return "", err
		}
		b, err := json.MarshalIndent(a.Record, "", "  ")
		if err != nil || len(b)+1 > workMax {
			return "", errors.New("work record is over 8 KB")
		}
		text := string(b) + "\n"
		req.Op, req.Version, req.Text = control.FilesWrite, a.Version, &text
	default:
		return "", errors.New("work op must be read or write")
	}
	ans, err := api.Files(ctx, req)
	if err != nil {
		return "", reason(err)
	}
	if ans.Refused != "" {
		return "", fmt.Errorf("work record unchanged: %s; read again before updating", ans.Refused)
	}
	if ans.File == nil {
		return "", errors.New("no work record returned")
	}
	if a.Op == "write" {
		return "Work record stored, version " + ans.File.Version + ". No wake, action or report was sent.", nil
	}
	var r WorkRecord
	if err := decodeWork([]byte(ans.File.Text), &r); err != nil {
		return "", err
	}
	if err := r.valid(); err != nil {
		return "", err
	}
	if err := workRefs(ctx, api, m.Scope, r); err != nil {
		return "", err
	}
	return fileText(*ans.File), nil
}

// workContext is added only after the existing wake gates admitted a turn.
// Missing opt-in data changes nothing; unreadable data remains visible as a
// blocked record without preventing unrelated existing wakes.
func (w *Waker) workContext(ctx context.Context, sv control.ThreadID) string {
	var dir string
	err := w.Core.Store.DB.QueryRowContext(ctx, `SELECT dir FROM supervisor_dirs WHERE scope = ?`, w.Scope).Scan(&dir)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	blocked := "\nDurable work record unavailable or invalid. Read notes/work.json with a file tool; retain it as blocked, do not assume completion or change its report destination.\n"
	if err != nil {
		return blocked
	}
	s := &ScopeFiles{Store: w.Core.Store, Log: w.Log}
	ans, err := s.do(ctx, w.Scope, sv, control.FilesRequest{Op: control.FilesRead, Path: WorkPath})
	if err == refusal("there is no such file") {
		return ""
	}
	if err != nil || ans.File == nil {
		return blocked
	}
	var r WorkRecord
	if decodeWork([]byte(ans.File.Text), &r) != nil || r.valid() != nil || r.ReportThread != sv {
		return blocked
	}
	api := w.Core.As(control.Caller{Principal: w.Owner, Scopes: []string{w.Scope}, May: control.OpRead})
	if workRefs(ctx, api, w.Scope, r) != nil {
		return blocked
	}
	b, _ := json.Marshal(r)
	text := "\nDurable work record (model-owned data, not authority or verified evidence), version " + ans.File.Version + ":\n> " + string(b) +
		"\nRead work_record before updating: this frozen wake may be stale. Stored completion is not report delivery.\n"
	if r.State == "complete" || r.State == "cancelled" {
		return text + "This terminal record is retained history: do not start another action or repeat its report without a new request.\n"
	}
	return text + "Assess the user's completion condition under current limits; retain blocks. Report via the ordinary final answer here, in the recorded supervisor thread.\n"
}
