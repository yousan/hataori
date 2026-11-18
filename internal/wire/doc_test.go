package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

const protocolDoc = "../../docs/protocol.md"

// row is one field as the document states it.
type row struct{ name, typ, presence string }

func (r row) String() string { return fmt.Sprintf("| `%s` | %s | %s |", r.name, r.typ, r.presence) }

// section is what the document says under one heading: its field table and
// its "Result:" line.
type section struct {
	rows   []row
	result string
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// typeName is how the document writes the JSON type of t.
func typeName(t reflect.Type) string {
	switch {
	case t == timeType:
		return "time"
	case t == rawType:
		return "any"
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return "string" // JSON carries []byte as base64
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Slice:
		return "[" + typeName(t.Elem()) + "]"
	case reflect.Struct:
		return "`" + docName(t) + "`"
	}
	return t.Kind().String()
}

// docName is the name of a struct type in the document.
func docName(t reflect.Type) string {
	n := t.Name()
	return strings.ToUpper(n[:1]) + n[1:]
}

// fields lists the JSON fields of struct t, embedded structs flattened, and
// adds the struct types they use to more.
func fields(t reflect.Type, more map[string]reflect.Type) []row {
	var out []row
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, fields(f.Type, more)...)
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		presence := "required"
		if strings.Contains(opts, "omitempty") {
			presence = "optional"
		}
		out = append(out, row{name, typeName(f.Type), presence})
		collect(f.Type, more)
	}
	return out
}

func collect(t reflect.Type, more map[string]reflect.Type) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct && t != timeType {
		more[docName(t)] = t
	}
}

// expected is what the document must say, by heading.
func expected() map[string]section {
	want := map[string]section{}
	types := map[string]reflect.Type{}
	for name, m := range map[string]any{"hello": hello{}, "request": request{}, "response": response{}} {
		want["Message `"+name+"`"] = section{rows: fields(reflect.TypeOf(m), types)}
	}
	for name, op := range operations {
		s := section{rows: fields(reflect.TypeOf(op.args), types), result: "none"}
		if op.result != nil {
			s.result = typeName(reflect.TypeOf(op.result))
			collect(reflect.TypeOf(op.result), types)
		}
		want["Operation `"+name+"`"] = s
	}
	for len(types) > 0 { // the types the others use, and the types those use
		var next = map[string]reflect.Type{}
		for name, t := range types {
			if _, done := want["Type `"+name+"`"]; !done {
				want["Type `"+name+"`"] = section{rows: fields(t, next)}
			}
		}
		for name := range next {
			if _, done := want["Type `"+name+"`"]; done {
				delete(next, name)
			}
		}
		types = next
	}
	for kind, fs := range control.EventKinds {
		var rows []row
		for _, f := range fs {
			p := "required"
			if f.Optional {
				p = "optional"
			}
			rows = append(rows, row{f.Name, string(f.Type), p})
		}
		want["Event `"+string(kind)+"`"] = section{rows: rows}
	}
	var codeRows []row
	for _, c := range codes {
		codeRows = append(codeRows, row{c.code, "error code", "-"})
	}
	codeRows = append(codeRows, row{"error", "error code", "-"})
	want["Error codes"] = section{rows: codeRows}
	return want
}

// parse reads the document's level-3 headings, the first three cells of
// each table row under them, and their "Result:" lines.
func parse(t *testing.T) map[string]section {
	t.Helper()
	b, err := os.ReadFile(protocolDoc)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]section{}
	head := ""
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "### "):
			head = strings.TrimSpace(strings.TrimPrefix(l, "### "))
			if _, dup := got[head]; dup {
				t.Errorf("heading %q twice", head)
			}
			got[head] = section{}
		case strings.HasPrefix(l, "## "):
			head = ""
		case head == "":
		case strings.HasPrefix(l, "Result: "):
			s := got[head]
			s.result = strings.TrimSpace(strings.TrimPrefix(l, "Result: "))
			got[head] = s
		case strings.HasPrefix(l, "| `"):
			cells := strings.Split(strings.Trim(l, "|"), "|")
			if len(cells) < 3 {
				t.Errorf("%s: short row %q", head, l)
				continue
			}
			s := got[head]
			s.rows = append(s.rows, row{strings.Trim(strings.TrimSpace(cells[0]), "`"), strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])})
			got[head] = s
		}
	}
	return got
}

func sortedRows(rs []row) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

// The document and the code name the same operations, messages, types,
// events and fields.
func TestProtocolDocMatchesTheCode(t *testing.T) {
	want, got := expected(), parse(t)
	var heads []string
	for h := range want {
		heads = append(heads, h)
	}
	sort.Strings(heads)
	for _, h := range heads {
		g, ok := got[h]
		w := want[h]
		if !ok {
			t.Errorf("docs/protocol.md has no section %q; it should read:\n### %s\n%s\nResult: %s", h, h, strings.Join(sortedRows(w.rows), "\n"), w.result)
			continue
		}
		if strings.Join(sortedRows(g.rows), "\n") != strings.Join(sortedRows(w.rows), "\n") {
			t.Errorf("%s: the document has\n%s\nthe code has\n%s", h, strings.Join(sortedRows(g.rows), "\n"), strings.Join(sortedRows(w.rows), "\n"))
		}
		if w.result != g.result && strings.HasPrefix(h, "Operation") {
			t.Errorf("%s: result %q in the document, %q in the code", h, g.result, w.result)
		}
	}
	for h := range got {
		if _, ok := want[h]; !ok && (strings.HasPrefix(h, "Operation") || strings.HasPrefix(h, "Type") ||
			strings.HasPrefix(h, "Event") || strings.HasPrefix(h, "Message")) {
			t.Errorf("docs/protocol.md describes %q, which the code does not have", h)
		}
	}
}

// Every text field of an event is optional.
func TestTextFieldsAreOptional(t *testing.T) {
	for kind, fs := range control.EventKinds {
		for _, f := range fs {
			if f.Text && !f.Optional {
				t.Errorf("event %s: text field %s is required", kind, f.Name)
			}
		}
	}
}
