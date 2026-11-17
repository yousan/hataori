package chat

import (
	"strings"
	"unicode"
)

// order is what a trigger message asks for.
type order struct {
	Repo   string
	Model  string // empty for the scope's default
	Effort string // empty for the scope's default
	Prompt string
}

// Efforts are the effort values hataori knows. An agent adapter maps each
// to what its agent takes.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// options are the names a trigger line may carry as name:value.
var options = map[string]bool{"repo": true, "model": true, "effort": true}

// parseTrigger reads a message that starts with the trigger word. The
// options follow the word on the same line, separated by spaces; the prompt
// is everything after the last option. A line break ends an option's value
// and the options. ok is false when text does not start with the word.
func parseTrigger(trigger, text string) (order, bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeftFunc(text, unicode.IsSpace), trigger)
	if !ok || trigger == "" || (rest != "" && !isSpace(rest[0])) {
		return order{}, false
	}
	opts := map[string]string{}
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" || rest[0] == '\n' || rest[0] == '\r' {
			break
		}
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			end = len(rest)
		}
		name, value, found := strings.Cut(rest[:end], ":")
		if !found || value == "" || !options[name] {
			break
		}
		if _, dup := opts[name]; dup {
			break
		}
		opts[name] = value
		rest = rest[end:]
	}
	return order{Repo: opts["repo"], Model: opts["model"], Effort: opts["effort"], Prompt: strings.TrimSpace(rest)}, true
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
