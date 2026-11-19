package codex

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a scrubbed capture must not contain.
var leaks = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a home directory path", regexp.MustCompile(`/home/[^/"\s\\]+|/Users/[^/"\s\\]+|-home-[A-Za-z0-9_]+-`)},
	// An interpreter in the home directory (a virtual environment): even
	// under /home/user, the rest of its path names the machine's own
	// directories.
	{"an interpreter in the home directory", regexp.MustCompile(`(/home|/Users)/[^"\s\\]*/bin/python[0-9.]*`)},
	{"an e-mail address", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	{"an API key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"a JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"a long hexadecimal string", regexp.MustCompile(`(?i)(^|[^0-9a-f])[0-9a-f]{32,}`)},
	{"an unscrubbed UUID", regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)},
}

// allowed are the replacements scrub.py writes.
var allowed = []*regexp.Regexp{
	regexp.MustCompile(`^/home/user$|^-home-user-$`),
	regexp.MustCompile(`^user\d+@example\.com$`),
	regexp.MustCompile(`^00000000-0000-4000-8000-\d{12}$`),
}

func isAllowed(s string) bool {
	for _, a := range allowed {
		if a.MatchString(s) {
			return true
		}
	}
	return false
}

func TestCapturesAreScrubbed(t *testing.T) {
	n := 0
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".md") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n++
		for _, m := range opaque.FindAllString(string(b), -1) {
			if looksLikeToken(m) {
				t.Errorf("%s contains something shaped like a token: %.12q...", path, m)
			}
		}
		for _, l := range leaks {
			for _, m := range l.re.FindAllString(string(b), -1) {
				if !isAllowed(m) {
					t.Errorf("%s contains %s: %.40q", path, l.what, m)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no captures found")
	}
}

// opaque finds runs of characters that tokens are made of.
var opaque = regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`)

// looksLikeToken tells a random string (a key, a session token, a
// signature) from a word, a name or a dashed path: it mixes upper case,
// lower case and digits, with digits making up at least a tenth of it.
func looksLikeToken(s string) bool {
	var upper, lower, digit int
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		case r >= '0' && r <= '9':
			digit++
		}
	}
	return upper > 0 && lower > 0 && digit*10 >= len(s)
}

func TestTokenShapes(t *testing.T) {
	for _, s := range []string{"sk1AbC9dEf8GhI7jKl6MnO5pQr4StU3vWx2YzA", "eyJhbGciOiJIUzI1NiJ9-x7Gk2Lm9Qp4Rt8Vw3Zy6"} {
		if !looksLikeToken(s) {
			t.Errorf("%q not taken for a token", s)
		}
	}
	for _, s := range []string{"-tmp-capture-work-sessions-background-plain", "mcp__claude_ai_Google_Calendar__respond_to_event", "00000000-0000-4000-8000-000000000001"} {
		if looksLikeToken(s) {
			t.Errorf("%q taken for a token", s)
		}
	}
}
