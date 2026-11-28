package workerproto

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	LoginCapability = "ephemeral_login"
	LoginStart      = "ephemeral_login_start"
	LoginInputOp    = "ephemeral_login_input"
	LoginCancel     = "ephemeral_login_cancel"
	MaxLoginData    = 16 << 10
)

// LoginData is ephemeral content carried inside LoginStream.Data. It is never
// a receipt, replay record, event or diagnostic. Result requires a clean exit
// and the resident's status probe; completion text alone cannot set it.
type LoginData struct {
	Link      string `json:"link,omitempty"`
	ShowCode  string `json:"show_code,omitempty"`
	WantsCode bool   `json:"wants_code,omitempty"`
	NoCode    bool   `json:"no_code,omitempty"`
	Refused   string `json:"refused,omitempty"`
	Result    string `json:"result,omitempty"` // logged_in or failed
}

// ValidLoginData bounds the live material independently of the frame limit.
func ValidLoginData(d LoginData) bool {
	if len(d.Link) > MaxLoginData || len(d.ShowCode) > MaxLoginData || len(d.Refused) > MaxLoginData ||
		!oneOf(d.Result, "", "logged_in", "failed") || d.NoCode && d.WantsCode {
		return false
	}
	if d.Result != "" {
		return d.Link == "" && d.ShowCode == "" && d.Refused == "" && !d.WantsCode
	}
	if d.Refused != "" && (d.Link != "" || d.ShowCode != "" || d.NoCode) || d.ShowCode != "" && !d.NoCode {
		return false
	}
	return d.Link != "" || d.ShowCode != "" || d.Refused != "" || d.WantsCode
}

func loginInput(b LoginInput) bool {
	return identifier(b.Attempt) && len(b.Data) > 0 && len(b.Data) <= MaxLoginData && utf8.Valid(b.Data) &&
		strings.IndexFunc(string(b.Data), func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) < 0
}

func DecodeLoginData(data []byte) (LoginData, error) {
	if !utf8.Valid(data) {
		return LoginData{}, refuse("invalid_message", "invalid login data encoding")
	}
	var d LoginData
	if err := decode(data, &d); err != nil {
		return LoginData{}, err
	}
	if !ValidLoginData(d) {
		return LoginData{}, refuse("invalid_message", "invalid login data")
	}
	return d, nil
}
