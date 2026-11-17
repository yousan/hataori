package discord

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Placeholders that scrub.py gives snowflakes: firstID and up.
const (
	firstID = 1000000000000000001
	lastID  = 1000000000000000999
)

var (
	word          = regexp.MustCompile(`[0-9A-Za-z]+`)
	digits        = regexp.MustCompile(`^[0-9]{17,20}$`)
	hex32         = regexp.MustCompile(`^(?:a_)?[0-9a-f]{32}$`)
	botTokenShape = regexp.MustCompile(`[A-Za-z0-9_-]{23,28}\.[A-Za-z0-9_-]{6,7}\.[A-Za-z0-9_-]{27,}`)
	longToken     = regexp.MustCompile(`[A-Za-z0-9_=-]{40,}`)
	signedURL     = regexp.MustCompile(`hm=([^&"]*)`)
	tokenValue    = regexp.MustCompile(`"token": "([^"]*)"`)
	email         = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// leaks lists what in a capture looks like a real identifier or a secret.
func leaks(data string) []string {
	var out []string
	for _, w := range word.FindAllString(data, -1) {
		if digits.MatchString(w) {
			if n, err := strconv.ParseUint(w, 10, 64); err != nil || n < firstID || n > lastID {
				out = append(out, "id outside the placeholder range: "+w)
			}
		}
		if hex32.MatchString(w) && !strings.HasPrefix(w, strings.Repeat("0", 24)) {
			out = append(out, "hash not replaced: "+w[:8]+"...")
		}
	}
	for _, s := range botTokenShape.FindAllString(data, -1) {
		out = append(out, "bot token shape: "+s[:8]+"...")
	}
	for _, s := range longToken.FindAllString(data, -1) {
		if mixed(s) {
			out = append(out, "token-like string: "+s[:8]+"...")
		}
	}
	for _, m := range signedURL.FindAllStringSubmatch(data, -1) {
		if m[1] != "REDACTED" {
			out = append(out, "signed URL not replaced")
		}
	}
	for _, m := range tokenValue.FindAllStringSubmatch(data, -1) {
		if !strings.HasPrefix(m[1], "REDACTED") {
			out = append(out, "token value not replaced")
		}
	}
	for _, s := range email.FindAllString(data, -1) {
		if !strings.HasSuffix(s, "@example.com") {
			out = append(out, "e-mail address: "+s)
		}
	}
	if strings.Contains(data, "/home/") {
		out = append(out, "home path")
	}
	return out
}

// mixed reports whether s has upper case, lower case and digits, as an
// encoded secret does and a run of one letter does not.
func mixed(s string) bool {
	var up, low, dig bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			low = true
		case r >= '0' && r <= '9':
			dig = true
		}
	}
	return up && low && dig
}

func TestCapturesAreScrubbed(t *testing.T) {
	files, _ := filepath.Glob("testdata/v10/*/*.json")
	if len(files) == 0 {
		t.Fatal("no captures")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range leaks(string(b)) {
			t.Errorf("%s: %s", f, l)
		}
	}
}

// Made-up secrets in the shapes the checks look for, built at run time from
// a placeholder id so that no secret-shaped literal is in the source.
func fakeBotToken() string {
	return base64.RawStdEncoding.EncodeToString([]byte("1000000000000000001")) + "." +
		"G" + strings.Repeat("x", 5) + "." + strings.Repeat("aB3", 10)
}

func fakeInteractionToken() string {
	return base64.RawStdEncoding.EncodeToString([]byte("interaction:1000000000000000001:" + strings.Repeat("Ab9", 6)))
}

// The checks catch the shapes they are for.
func TestLeakShapes(t *testing.T) {
	for _, s := range []string{
		`"id": "1999999999999999999"`,
		`"x": "` + fakeBotToken() + `"`,
		`"avatar": "5f2b9c0e4d7a1b3c8e6f0a2d4b6c8e0f"`,
		`"token": "` + fakeInteractionToken() + `"`,
		`"url": "https://cdn.discordapp.com/a/b/c.txt?ex=1&is=2&hm=3f4e"`,
		`"email": "someone@corp.example"`,
		`"path": "/home/someone/x"`,
	} {
		if len(leaks(s)) == 0 {
			t.Errorf("not caught: %s", s)
		}
	}
	for _, s := range []string{
		`"id": "1000000000000000042"`,
		`"content": "` + strings.Repeat("x", 2001) + `"`,
		`"session_id": "00000000000000000000000000000003"`,
		`"token": "REDACTED-TOKEN-1"`,
		`"url": "https://cdn.discordapp.com/a/b/c.txt?ex=00000000&is=00000000&hm=REDACTED&"`,
	} {
		if l := leaks(s); len(l) != 0 {
			t.Errorf("%s: %v", s, l)
		}
	}
}
