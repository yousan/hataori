package supervisor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// filesAPI answers Files, and keeps what it was asked.
type filesAPI struct {
	control.API
	ans   control.FilesAnswer
	err   error
	asked []control.FilesRequest
}

func (a *filesAPI) Files(_ context.Context, r control.FilesRequest) (control.FilesAnswer, error) {
	a.asked = append(a.asked, r)
	return a.ans, a.err
}

// Each file tool asks for what its arguments say and no more, and its
// answer says what was done, or what is there instead.
func TestFileTools(t *testing.T) {
	text := func(s string) *string { return &s }
	file := func(path, text string) *control.ScopeFile {
		return &control.ScopeFile{Path: path, Size: int64(len(text)), Version: versionOf(text), Text: text}
	}
	long := strings.Repeat("x", FileTextMax)
	for _, c := range []struct {
		tool, args string
		ans        control.FilesAnswer
		asked      control.FilesRequest
		want       string
	}{
		{ToolFiles, `{}`, control.FilesAnswer{}, control.FilesRequest{Op: control.FilesList}, "No files in your directory."},
		{ToolFiles, `null`, control.FilesAnswer{}, control.FilesRequest{Op: control.FilesList}, "No files in your directory."},
		{ToolFiles, `{"path":"notes"}`, control.FilesAnswer{Files: []control.ScopeFile{{Path: "AGENTS.md", Size: 5, Version: "v"}, {Path: "bin", Size: 7}}},
			control.FilesRequest{Op: control.FilesList},
			"2 files in your directory:\nAGENTS.md (5 bytes)\nbin (7 bytes, not for these tools: too large, or not UTF-8 text)\n"},
		{ToolFile, `{"path":"notes/plan.md","text":"x","version":"v","old":"o","new":"n"}`, control.FilesAnswer{File: file("notes/plan.md", "a\nb\n")},
			control.FilesRequest{Op: control.FilesRead, Path: "notes/plan.md"},
			"notes/plan.md (4 bytes), version " + versionOf("a\nb\n") + ". Its text is all that follows the next line.\n---\na\nb\n"},
		{ToolFile, `{"path":"long.md"}`, control.FilesAnswer{File: file("long.md", long)}, control.FilesRequest{Op: control.FilesRead, Path: "long.md"},
			fmt.Sprintf("long.md (%d bytes), version %s. Its text is all that follows the next line.\n---\n%s", FileTextMax, versionOf(long), long)},
		{ToolFile, `{"path":"longer.md"}`, control.FilesAnswer{File: file("longer.md", long+"x")}, control.FilesRequest{Op: control.FilesRead, Path: "longer.md"},
			fmt.Sprintf("longer.md (%d bytes): longer than the %d bytes that this tool shows. Read it in parts with your other file tools, and edit it in parts.", FileTextMax+1, FileTextMax)},
		{ToolWrite, `{"path":"notes/plan.md","text":"","old":"o","new":"n"}`, control.FilesAnswer{File: file("notes/plan.md", "")},
			control.FilesRequest{Op: control.FilesWrite, Path: "notes/plan.md", Text: text("")}, "Written: notes/plan.md (0 bytes), version " + versionOf("") + "."},
		{ToolWrite, `{"path":"notes/plan.md","text":"a\nb","version":"v1"}`, control.FilesAnswer{File: file("notes/plan.md", "a\nb")},
			control.FilesRequest{Op: control.FilesWrite, Path: "notes/plan.md", Text: text("a\nb"), Version: "v1"}, "Written: notes/plan.md (3 bytes), version " + versionOf("a\nb") + "."},
		{ToolWrite, `{"path":"plan.md","text":"x"}`, control.FilesAnswer{File: file("plan.md", "there"), Refused: "the file is there already"},
			control.FilesRequest{Op: control.FilesWrite, Path: "plan.md", Text: text("x")},
			"Nothing was written: the file is there already. What is there now:\nplan.md (5 bytes), version " + versionOf("there") + ". Its text is all that follows the next line.\n---\nthere"},
		{ToolWrite, `{"path":"plan.md","text":"x","version":"v"}`, control.FilesAnswer{File: file("plan.md", long+"x"), Refused: "the file has changed since that version"},
			control.FilesRequest{Op: control.FilesWrite, Path: "plan.md", Text: text("x"), Version: "v"},
			fmt.Sprintf("Nothing was written: the file has changed since that version. What is there now:\nplan.md (%d bytes): longer than the %d bytes that this tool shows. Read it in parts with your other file tools, and edit it in parts.", FileTextMax+1, FileTextMax)},
		{ToolWrite, `{"path":"plan.md","text":"x","version":"v"}`, control.FilesAnswer{Refused: "there is no such file any more"},
			control.FilesRequest{Op: control.FilesWrite, Path: "plan.md", Text: text("x"), Version: "v"},
			"Nothing was written: there is no such file any more. No file is there now."},
		{ToolEdit, `{"path":"plan.md","old":" a ","new":"","text":"x","version":"v"}`, control.FilesAnswer{File: file("plan.md", "xyz")},
			control.FilesRequest{Op: control.FilesEdit, Path: "plan.md", Old: " a ", New: text("")}, "Written: plan.md (3 bytes). To write the whole file, read it first."},
		{ToolEdit, `{"path":"bin","old":"a","new":"b"}`, control.FilesAnswer{File: &control.ScopeFile{Path: "bin", Size: 9}, Refused: "the file is over 256 KB, or not UTF-8"},
			control.FilesRequest{Op: control.FilesEdit, Path: "bin", Old: "a", New: text("b")},
			"Nothing was written: the file is over 256 KB, or not UTF-8. What is there now:\nbin (9 bytes): not for these tools, too large, or not UTF-8 text."},
	} {
		api := &filesAPI{ans: c.ans}
		m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123"}
		got, isErr := call(t, m, c.tool, c.args)
		if isErr || got != c.want {
			t.Errorf("%s %.80s: answered %v\n%.400s\nwant\n%.400s", c.tool, c.args, isErr, got, c.want)
		}
		if len(api.asked) != 1 || !reflect.DeepEqual(api.asked[0], c.asked) {
			t.Errorf("%s %.80s: asked %+v, want %+v", c.tool, c.args, api.asked, c.asked)
		}
	}
}

// A call without what its tool needs, or whose text encoding/json would
// change, asks for nothing; and a refusal is an error that says why.
func TestFileToolsRefuse(t *testing.T) {
	const path = "bad arguments: path is required"
	const edit = "bad arguments: old and new are required, and old is not empty"
	for _, c := range []struct{ tool, args, want string }{
		{ToolFile, `{}`, path},
		{ToolFile, `null`, path},
		{ToolFile, `{"path":""}`, path},
		{ToolWrite, `{"text":"x"}`, path},
		{ToolEdit, `{"old":"x","new":"y"}`, path},
		{ToolWrite, `{"path":"plan.md"}`, "bad arguments: text is required"},
		{ToolWrite, `{"path":"plan.md","text":null}`, "bad arguments: text is required"},
		{ToolEdit, `{"path":"plan.md","new":"y"}`, edit},
		{ToolEdit, `{"path":"plan.md","old":"","new":"y"}`, edit},
		{ToolEdit, `{"path":"plan.md","old":"x"}`, edit},
		{ToolWrite, "{\"path\":\"plan.md\",\"text\":\"a\xffb\"}", "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":"a\ud83d"}`, "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":"a\ud83d\u0041"}`, "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":"a\ud83d\\udc49"}`, "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":"a\ud83d\ud83d\udc49"}`, "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":"\udc49\ud83d"}`, "bad arguments: not UTF-8"},
		{ToolEdit, `{"path":"plan.md","old":"x","new":"\uDC49 "}`, "bad arguments: not UTF-8"},
		{ToolWrite, `{"path":"plan.md","text":7}`, "bad arguments: json"},
		{ToolWrite, `{"path":["plan.md"],"text":"x"}`, "bad arguments: json"},
	} {
		api := &filesAPI{}
		m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123"}
		got, isErr := call(t, m, c.tool, c.args)
		if !isErr {
			t.Errorf("%s %s: no error", c.tool, c.args)
		}
		if !strings.HasPrefix(got, c.want) || len(api.asked) != 0 {
			t.Errorf("%s %s: answered %q and asked %+v, want %q", c.tool, c.args, got, api.asked, c.want)
		}
	}
	// What is escaped whole is taken as it is.
	api := &filesAPI{ans: control.FilesAnswer{File: &control.ScopeFile{Path: "plan.md"}}}
	m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123"}
	if got, isErr := call(t, m, ToolWrite, `{"path":"plan.md","text":"\ud83d\udc49 \uD83D\uDC49 \\ud83d \u3042 \\\" \ufffd"}`); isErr || len(api.asked) != 1 || *api.asked[0].Text != "👉 👉 \\ud83d あ \\\" \ufffd" {
		t.Errorf("answered %q, asked %+v", got, api.asked)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{errors.New("supervisor: Files: no write in docs/"), "no write in docs/"},
		{control.ErrNotSupported, control.ErrNotSupported.Error()},
	} {
		api := &filesAPI{err: c.err}
		m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123"}
		if got, isErr := call(t, m, ToolWrite, `{"path":"docs/x","text":"x"}`); !isErr || !strings.Contains(got, c.want) {
			t.Errorf("refused with %v: answered %v %q", c.err, isErr, got)
		}
	}
	m = &MCP{Dial: func() (control.API, error) { return &filesAPI{}, nil }, Scope: "C0123"}
	if got, isErr := call(t, m, ToolWrite, `{"path":"plan.md","text":"x"}`); !isErr {
		t.Errorf("an answer that is none: %q", got)
	}
}
