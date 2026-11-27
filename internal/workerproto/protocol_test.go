package workerproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func encoded(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func message(t *testing.T, name string) Message {
	t.Helper()
	return examples(t)[name].(Message)
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != want {
		t.Fatalf("got %v, want refusal %s", err, want)
	}
}

func TestMajorRefusedBeforeEffects(t *testing.T) {
	peer := hello()
	peer.Version = "2.9"
	c, err := Negotiate(registration, hello(), peer)
	var r *VersionRefusal
	if c != nil || !errors.As(err, &r) || r.Local != Version || r.Peer != peer.Version || r.Refusal.Code != "incompatible_major" {
		t.Fatalf("got %v, %v", c, err)
	}
	if !strings.Contains(err.Error(), Version) || !strings.Contains(err.Error(), peer.Version) {
		t.Fatal("version refusal must name both versions")
	}
}

func TestMinorCapabilitiesAndExtensions(t *testing.T) {
	local, peer := hello(), hello()
	local.Version, peer.Version = "1.3", "1.1"
	peer.Capabilities = []string{"health", "future"}
	c, err := Negotiate(registration, local, peer)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Hello()
	if h.Version != "1.1" || len(h.Capabilities) != 1 || h.Capabilities[0] != "health" {
		t.Fatalf("unexpected negotiation: %+v", h)
	}
	m := message(t, "request-health.json")
	b := bytes.TrimSpace(encoded(t, m))
	b = append(b[:len(b)-1], []byte(`,"future_optional":{"value":true}}`)...)
	if _, err := c.Decode(b, nil); err != nil {
		t.Fatalf("optional extension refused: %v", err)
	}
	m.Op = "future"
	_, err = c.Decode(encoded(t, m), nil)
	code(t, err, "unsupported_operation")
	m = message(t, "request-input.json")
	_, err = c.Decode(encoded(t, m), &target)
	code(t, err, "unsupported_capability")
	peer.Required = []string{"future"}
	_, err = Negotiate(registration, local, peer)
	code(t, err, "unsupported_capability")
}

func TestDurableCapabilitiesRequireReceiptRecovery(t *testing.T) {
	for _, capability := range []string{"host", "workspace", "transfer", "replay"} {
		peer := hello()
		peer.Capabilities = []string{capability}
		c, err := Negotiate(registration, hello(), peer)
		if c != nil {
			t.Fatal("durable operations negotiated without receipt recovery")
		}
		code(t, err, "unsupported_capability")
	}
}

func TestRefusalsOfUnknownOperationsAndCapabilities(t *testing.T) {
	peer := hello()
	peer.Capabilities = []string{"health"}
	c, err := Negotiate(registration, hello(), peer)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"future", "input"} {
		m := message(t, "refusal-input.json")
		m.Op = op
		if _, err := c.Decode(encoded(t, m), &target); err != nil {
			t.Fatalf("cannot decode refusal of %s: %v", op, err)
		}
		m.Target.Run = "other-run"
		_, err := c.Decode(encoded(t, m), &target)
		code(t, err, "wrong_target")
	}
}

func TestRegistrationCannotBeReplacedByHello(t *testing.T) {
	for _, field := range []string{"main", "worker"} {
		t.Run(field, func(t *testing.T) {
			peer := hello()
			if field == "main" {
				peer.Binding.Main = "other-main"
			} else {
				peer.Binding.Worker = "replacement-worker"
			}
			c, err := Negotiate(registration, hello(), peer)
			if c != nil {
				t.Fatal("mismatched hello yielded an authorized connection")
			}
			code(t, err, "binding")
		})
	}
	for _, v := range []string{"", "1", "01.0", "1.-1", "0.0", "1.0.0", "1.1000000", "1.+2"} {
		peer := hello()
		peer.Version = v
		_, err := Negotiate(registration, hello(), peer)
		code(t, err, "invalid_message")
	}
	for _, field := range []string{"owner", "person", "principal", "supervisor", "token", "role"} {
		data := bytes.TrimSpace(encoded(t, hello()))
		data = append(data[:len(data)-1], []byte(`,"`+field+`":"untrusted"}`)...)
		_, err := DecodeHello(data)
		code(t, err, "invalid_message")
	}
}

func TestOperationAndReportIdentityBeforeDispatch(t *testing.T) {
	c := connection(t)
	for _, kind := range []string{"request", "reply", "stream", "refusal"} {
		for _, identity := range []string{"main", "worker", "thread", "session", "run"} {
			t.Run(kind+"-"+identity, func(t *testing.T) {
				fixture := kind + "-read.json"
				if kind == "stream" {
					fixture = "stream-read-data.json"
				}
				m := message(t, fixture)
				switch identity {
				case "main":
					m.Binding.Main = "other-main"
				case "worker":
					m.Binding.Worker = "other-worker"
				case "thread":
					m.Target.Thread++
				case "session":
					m.Target.Session = "other-session"
				case "run":
					m.Target.Run = "old-run"
				}
				effects := 0
				_, err := c.Decode(encoded(t, m), &target)
				if err == nil {
					effects++ // The dispatcher must run only after Decode succeeds.
				}
				if effects != 0 {
					t.Fatal("identity guard allowed dispatch")
				}
				want := "wrong_target"
				if identity == "main" || identity == "worker" {
					want = "binding"
				}
				code(t, err, want)
			})
		}
	}
	m := message(t, "request-input.json")
	_, err := c.Decode(encoded(t, m), nil)
	code(t, err, "wrong_target")
	m.Target = nil
	_, err = c.Decode(encoded(t, m), &target)
	code(t, err, "wrong_target")
	m = message(t, "request-health.json")
	m.Target = &target
	_, err = c.Decode(encoded(t, m), nil)
	code(t, err, "wrong_target")
	inv := Inventory{Boot: "boot-A", Complete: true, Hosts: []HostRecord{{Target: target}}}
	if err := CheckInventory([]Target{target}, inv); err != nil {
		t.Fatal(err)
	}
	inv.Hosts[0].Target.Thread++
	code(t, CheckInventory([]Target{target}, inv), "wrong_target")
}

func TestAddressesAndAuthorityNeverReachDispatch(t *testing.T) {
	c := connection(t)
	for _, key := range []string{"path", "socket", "address", "main_socket", "worker_socket", "command", "env", "pid", "caller", "owner", "token", "Socket"} {
		t.Run(key, func(t *testing.T) {
			m := message(t, "request-input.json")
			m.Body = json.RawMessage(`{"turn":"turn-A","data":"eA==","extension":{"` + key + `":"/untrusted/resource"}}`)
			_, err := c.Decode(encoded(t, m), &target)
			code(t, err, "invalid_message")
		})
	}
	for _, op := range []string{"start", "follow", "remove", "restore", "download", "attach"} {
		m := message(t, "request-"+op+".json")
		var b map[string]any
		if err := json.Unmarshal(m.Body, &b); err != nil {
			t.Fatal(err)
		}
		for key := range b {
			if oneOf(key, "workspace", "source", "trash", "handle") {
				b[key] = "/untrusted/resource"
			}
		}
		m.Body = raw(t, b)
		_, err := c.Decode(encoded(t, m), &target)
		code(t, err, "invalid_message")
	}
}

func TestMalformedFramesRefuse(t *testing.T) {
	c := connection(t)
	valid := encoded(t, message(t, "request-health.json"))
	for _, b := range [][]byte{
		nil, []byte("null"), []byte("[]"), []byte("{"), append(append([]byte{}, valid...), []byte("{}")...),
		[]byte(`{"kind":"request","kind":"reply"}`),
		bytes.Repeat([]byte("x"), MaxFrame+1),
		[]byte(`{"extension":` + strings.Repeat("[", 40) + `0` + strings.Repeat("]", 40) + `}`),
	} {
		if _, err := c.Decode(b, nil); err == nil {
			t.Fatalf("accepted malformed frame %.80q", b)
		}
	}
	m := message(t, "request-input.json")
	m.Key = ""
	_, err := c.Decode(encoded(t, m), &target)
	code(t, err, "invalid_message")
	m = message(t, "request-input.json")
	m.Body = raw(t, Input{Turn: "turn-A", Data: bytes.Repeat([]byte("x"), MaxChunk+1)})
	_, err = c.Decode(encoded(t, m), &target)
	code(t, err, "invalid_message")
}

func TestCaseAliasesRefuseBeforeDispatch(t *testing.T) {
	c := connection(t)
	remove := message(t, "request-remove.json")
	for _, payload := range []string{
		`{"workspace":"workspace-A","WORKSPACE":"workspace-B"}`,
		`{"WORKSPACE":"workspace-B","workspace":"workspace-A"}`,
		`{"WORKSPACE":"workspace-B"}`,
		`{"WorkSpace":"workspace-B"}`,
		`{"workspace":"workspace-A","\u0057ORKSPACE":"workspace-B"}`,
		`{"workſpace":"workspace-B"}`, // Unicode simple-fold alias accepted by encoding/json.
	} {
		t.Run(payload, func(t *testing.T) {
			m := remove
			m.Body = json.RawMessage(payload)
			effects := 0
			_, err := c.Decode(encoded(t, m), &target)
			if err == nil {
				effects++
			}
			if effects != 0 {
				t.Fatal("case-variant workspace reached dispatch")
			}
			code(t, err, "invalid_message")
		})
	}
	for _, fixture := range []struct {
		name, before, after string
	}{
		{"request-remove.json", `"op":"remove"`, `"op":"remove","OP":"inspect"`},
		{"request-remove.json", `"main":"main-A"`, `"main":"main-A","MAIN":"main-B"`},
		{"request-remove.json", `"run":"run-A"`, `"run":"run-A","RUN":"run-B"`},
		{"reply-remove.json", `"state":"complete"`, `"state":"complete","STATE":"unknown"`},
		{"stream-read-data.json", `"source":"events"`, `"source":"events","SOURCE":"other"`},
	} {
		t.Run(fixture.name+"-"+fixture.before, func(t *testing.T) {
			data := encoded(t, message(t, fixture.name))
			if !bytes.Contains(data, []byte(fixture.before)) {
				t.Fatal("fixture does not contain the field being tested")
			}
			data = bytes.Replace(data, []byte(fixture.before), []byte(fixture.after), 1)
			_, err := c.Decode(data, &target)
			code(t, err, "invalid_message")
		})
	}
	h := bytes.TrimSpace(encoded(t, hello()))
	h = append(h[:len(h)-1], []byte(`,"VERSION":"2.0"}`)...)
	_, err := DecodeHello(h)
	code(t, err, "invalid_message")
	// Lowercase extensions remain inert, including inside operation bodies.
	remove.Body = json.RawMessage(`{"workspace":"workspace-A","future_optional":{"value":true}}`)
	if _, err := c.Decode(encoded(t, remove), &target); err != nil {
		t.Fatalf("canonical optional extension was refused: %v", err)
	}
}

func TestReceiptCorrelatesOriginalRequest(t *testing.T) {
	m := message(t, "request-input.json")
	hash, err := RequestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	r := Receipt{Operation: "input", Key: m.Key, RequestHash: hash, State: "unknown"}
	if err := CheckReceipt(m, r); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"key", "operation", "hash", "state", "body", "target"} {
		t.Run(change, func(t *testing.T) {
			copy, req := r, m
			switch change {
			case "key":
				copy.Key = "other-key"
			case "operation":
				copy.Operation = "answer"
			case "hash":
				copy.RequestHash = strings.Repeat("0", 64)
			case "state":
				copy.Result = &Effect{}
			case "body":
				req.Body = raw(t, Input{Turn: "turn-A", Data: []byte("different")})
			case "target":
				newTarget := target
				newTarget.Run = "new-run"
				req.Target = &newTarget
			}
			code(t, CheckReceipt(req, copy), "key_conflict")
		})
	}
	m.ID = "transport-retry"
	m.Body = json.RawMessage(`{ "data": "UnVuIHRlc3RzLgo=", "turn": "turn-A" }`)
	hash2, err := RequestHash(m)
	if err != nil || hash2 != hash {
		t.Fatal("transport ID, whitespace or object order changed the receipt hash")
	}
	for _, op := range []string{"keys", "login_start", "login_input", "login_cancel"} {
		_, err := RequestHash(message(t, "request-"+op+".json"))
		code(t, err, "invalid_message")
	}
}

func TestReplayRangesMustMatchRecordedRead(t *testing.T) {
	req := Read{Source: src, Order: 1, Limit: MaxChunk}
	b := Bytes{Source: src, Order: 1, Next: 3, State: "data", Data: []byte("abc")}
	if err := CheckRange(req, b); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"handle", "source", "generation", "order", "offset", "next", "limit", "empty_with_data"} {
		t.Run(change, func(t *testing.T) {
			copy, request := b, req
			switch change {
			case "handle":
				copy.Handle = "other-handle"
			case "source":
				copy.Source.Source = "other-source"
			case "generation":
				copy.Generation = "new-generation"
			case "order":
				copy.Order++
			case "offset":
				copy.Offset, copy.Next = 1, 4
			case "next":
				copy.Next++
			case "limit":
				request.Limit = 2
			case "empty_with_data":
				copy.State = "empty"
			}
			code(t, CheckRange(request, copy), "invalid_message")
		})
	}
	for _, state := range []string{"empty", "unavailable", "missing", "unreadable", "replaced", "truncated", "history_lost"} {
		copy := Bytes{Source: src, Order: 1, State: state}
		if err := CheckRange(req, copy); err != nil {
			t.Fatalf("distinct zero-byte state %s rejected: %v", state, err)
		}
	}
}
