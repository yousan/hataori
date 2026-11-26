package control

import "strconv"

// IncomingFile is an incoming service-listed file. Only the daemon's chat front
// end sets these references; control socket input cannot supply them.
type IncomingFile struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// InputFile reports a delivered or skipped attachment, without its signed URL.
// Since protocol 3.43, in the input and answer events. Path or Reason is set.
type InputFile struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func FileLines(files []InputFile) string {
	var text string
	for _, f := range files {
		if f.Reason != "" {
			text += "\nAttachment " + strconv.Quote(f.Name) + " skipped: " + f.Reason
		} else {
			text += "\nAttachment " + strconv.Quote(f.Name) + ": " + strconv.Quote(f.Path)
		}
	}
	return text
}
