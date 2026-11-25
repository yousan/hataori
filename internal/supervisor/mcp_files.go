package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// FileTextMax is the most bytes of a file's text that the file tools
// return, enough for each of the user's files. A longer text is left out
// with its version: what was not read whole is not to be written over.
const FileTextMax = UserFileMax

// fileOps are the operations of the file tools.
var fileOps = map[string]string{
	ToolFiles: control.FilesList, ToolFile: control.FilesRead, ToolWrite: control.FilesWrite, ToolEdit: control.FilesEdit,
}

var filePathArg = map[string]any{"type": "string", "description": "the file's path in your directory, such as notes/plan.md"}

// file runs one of the file tools. What does not fit the file is an answer
// and no error: an agent may cut an error's text, and the file's is in it.
func (m *MCP) file(ctx context.Context, op string, raw json.RawMessage) (string, error) {
	var a struct {
		Path    string  `json:"path"`
		Text    *string `json:"text"`
		Version string  `json:"version"`
		Old     string  `json:"old"`
		New     *string `json:"new"`
	}
	if !utf8.Valid(raw) || halfRune(raw) { // encoding/json would change it
		return "", errors.New("bad arguments: not UTF-8")
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	r := control.FilesRequest{Op: op}
	if op != control.FilesList {
		if a.Path == "" {
			return "", errors.New("bad arguments: path is required")
		}
		r.Path = a.Path
	}
	switch op {
	case control.FilesWrite:
		if a.Text == nil {
			return "", errors.New("bad arguments: text is required")
		}
		r.Text, r.Version = a.Text, a.Version
	case control.FilesEdit:
		if a.Old == "" || a.New == nil {
			return "", errors.New("bad arguments: old and new are required, and old is not empty")
		}
		r.Old, r.New = a.Old, a.New
	}
	api, err := m.connect()
	if err != nil {
		return "", err
	}
	ans, err := api.Files(ctx, r)
	if err != nil {
		return "", reason(err)
	}
	switch {
	case op == control.FilesList:
		return cut(filesText(ans.Files), ForgeMax), nil
	case ans.Refused != "" && ans.File == nil:
		return "Nothing was written: " + ans.Refused + ". No file is there now.", nil
	case ans.Refused != "":
		return "Nothing was written: " + ans.Refused + ". What is there now:\n" + fileText(*ans.File), nil
	case ans.File == nil:
		return "", errors.New("failed: hataori answered nothing")
	case op == control.FilesRead:
		return fileText(*ans.File), nil
	case op == control.FilesWrite:
		return fmt.Sprintf("Written: %s (%d bytes), version %s.", ans.File.Path, ans.File.Size, ans.File.Version), nil
	}
	// Without the version: the rest of the file may be another's, unread.
	return fmt.Sprintf("Written: %s (%d bytes). To write the whole file, read it first.", ans.File.Path, ans.File.Size), nil
}

// halfRune reports whether a string of raw, JSON, has one half of a
// character that is escaped as two.
func halfRune(raw []byte) bool {
	unit := func(i int) (rune, bool) { // the \u escape that begins at i
		if len(raw) < i+6 || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		n, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		return rune(n), err == nil
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if r, ok := unit(i); ok && utf16.IsSurrogate(r) {
			next, ok := unit(i + 6)
			if !ok || utf16.DecodeRune(r, next) == utf8.RuneError {
				return true
			}
			i += 10
		}
		i++ // what it escapes
	}
	return false
}

// filesText is the list of fs.
func filesText(fs []control.ScopeFile) string {
	if len(fs) == 0 {
		return "No files in your directory."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d files in your directory:\n", len(fs))
	for _, f := range fs {
		note := ""
		if f.Version == "" {
			note = ", not for these tools: too large, or not UTF-8 text"
		}
		fmt.Fprintf(&b, "%s (%d bytes%s)\n", f.Path, f.Size, note)
	}
	return b.String()
}

// fileText is f, with its text and version when they are given and not too
// long.
func fileText(f control.ScopeFile) string {
	head := fmt.Sprintf("%s (%d bytes", f.Path, f.Size)
	switch {
	case f.Version == "":
		return head + "): not for these tools, too large, or not UTF-8 text."
	case len(f.Text) > FileTextMax:
		return head + fmt.Sprintf("): longer than the %d bytes that this tool shows. Read it in parts with your other file tools, and edit it in parts.", FileTextMax)
	}
	return head + "), version " + f.Version + ". Its text is all that follows the next line.\n---\n" + f.Text
}
