package workerproto

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
	return len(d.Link) <= 16384 && len(d.ShowCode) <= 16384 && len(d.Refused) <= 16384 &&
		(d.Result == "" || d.Result == "logged_in" || d.Result == "failed") && !(d.NoCode && d.WantsCode)
}

func DecodeLoginData(data []byte) (LoginData, error) {
	var d LoginData
	if err := decode(data, &d); err != nil {
		return LoginData{}, err
	}
	if !ValidLoginData(d) {
		return LoginData{}, refuse("invalid_message", "invalid login data")
	}
	return d, nil
}
