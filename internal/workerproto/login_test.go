package workerproto

import "testing"

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
