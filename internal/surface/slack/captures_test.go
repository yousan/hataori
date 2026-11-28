package slack

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestCaptureTools(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "testdata", "-p", "test_capture.py")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture tool checks: %v\n%s", err, out)
	}
}

var (
	slackID    = regexp.MustCompile(`\b(?:[UTCAFBGDWE][A-Z0-9]{8,}|Ev[A-Za-z0-9]{8,})\b`)
	tokenShape = regexp.MustCompile(`(?:xox[bpe]-|xapp-)[A-Za-z0-9-]+`)
	urlShape   = regexp.MustCompile(`(?:https?|wss)://[^\s<>|"\\]+`)
	emailShape = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// Never include the matching value in failures: it might be a credential.
func captureLeaks(data string) []string {
	var found []string
	if tokenShape.MatchString(data) {
		found = append(found, "token-shaped string")
	}
	for _, id := range slackID.FindAllString(data, -1) {
		num := id[1:]
		if strings.HasPrefix(id, "Ev") {
			num = id[2:]
		}
		n, err := strconv.Atoi(num)
		if err != nil || len(num) != 10 || n < 1 || n > 999 {
			found = append(found, "ID outside placeholder range")
		}
	}
	for _, u := range urlShape.FindAllString(data, -1) {
		if !regexp.MustCompile(`^https://example\.invalid/redacted-url-[0-9]+$`).MatchString(u) {
			found = append(found, "unredacted URL")
		}
	}
	for _, mail := range emailShape.FindAllString(data, -1) {
		if !regexp.MustCompile(`^user[0-9]+@example\.com$`).MatchString(mail) {
			found = append(found, "unredacted email")
		}
	}
	if strings.Contains(data, "/home/") || strings.Contains(data, "/Users/") {
		found = append(found, "home path")
	}
	return found
}

func TestCapturesAreScrubbed(t *testing.T) {
	files, err := filepath.Glob("testdata/captures/*/*.json")
	if err != nil || len(files) == 0 {
		t.Fatal("no captures")
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(b) {
			t.Errorf("%s: invalid JSON", path)
		}
		for _, leak := range captureLeaks(string(b)) {
			t.Errorf("%s: %s", path, leak)
		}
	}
}

func TestLeakShapes(t *testing.T) {
	for _, prefix := range []string{"xoxb", "xoxp", "xapp", "xoxe"} {
		if len(captureLeaks(prefix+"-"+strings.Repeat("Ab9", 6))) == 0 {
			t.Errorf("missed %s token shape", prefix)
		}
	}
	for _, s := range []string{
		"UABCDEF1234", "https://files.slack.com/files-pri/test", "https://example.com/?signature=secret",
		"someone@corp.example", "/home/someone/file", "/Users/someone/file",
		`<https://files.slack.com/files-pri/test|test link label>`,
		`<https://example.invalid/redacted-url-1|https://files.slack.com/files-pri/test>`,
		`<https://example.invalid/redacted-url-1?signature=secret|test link label>`,
	} {
		if len(captureLeaks(s)) == 0 {
			t.Error("missed leak shape")
		}
	}
	for _, s := range []string{
		`U0000000001 Ev0000000002 https://example.invalid/redacted-url-1 user1@example.com`,
		`<https://example.invalid/redacted-url-1|test link label>`,
	} {
		if len(captureLeaks(s)) != 0 {
			t.Fatal("rejected placeholders")
		}
	}
}
