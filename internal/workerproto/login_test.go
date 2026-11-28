package workerproto

import "testing"

func TestEphemeralLoginDataUTF8(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"invalid-byte", []byte("{\"show_code\":\"\xff\",\"no_code\":true}"), false},
		{"multibyte", []byte(`{"show_code":"表示-code","no_code":true}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := DecodeLoginData(tc.data)
			if (err == nil) != tc.valid {
				t.Errorf("UTF-8 login payload validity = %v, want %v", err == nil, tc.valid)
			}
			if tc.valid && d.ShowCode != "表示-code" {
				t.Error("valid multibyte login code changed")
			}
			m := message(t, "stream-ephemeral-login-device.json")
			m.Body = raw(t, LoginStream{Attempt: "attempt-A", State: "data", Data: tc.data})
			if _, err := connection(t).Decode(encoded(t, m), nil); (err == nil) != tc.valid {
				t.Errorf("typed stream UTF-8 validity = %v, want %v", err == nil, tc.valid)
			}
		})
	}
}

func TestEphemeralLoginDataRefusesAmbiguousPayloads(t *testing.T) {
	for _, data := range []string{
		`{"result":"logged_in","result":"failed"}`,
		`{"Result":"logged_in"}`,
		`{"no_code":true,"wants_code":true}`,
		`{"result":"unknown"}`,
		`{"link":"x"} {"result":"logged_in"}`,
		`{"person":"owner"}`,
	} {
		if _, err := DecodeLoginData([]byte(data)); err == nil {
			t.Fatalf("accepted ambiguous login payload %s", data)
		}
	}
	if _, err := DecodeLoginData([]byte(`{"no_code":true,"show_code":"fixture-code"}`)); err != nil {
		t.Fatal(err)
	}
}
