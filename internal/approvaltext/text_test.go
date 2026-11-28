package approvaltext

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestApprovalTextCharactersAndPositions(t *testing.T) {
	for _, text := range []string{"", "hello\n\t世界", "👩🏽‍💻", "👨‍👩‍👧‍👦", "❤️", "1️⃣", "✈️", "🇯🇵", strings.Repeat("a", MaxBytes)} {
		if err := Validate(text); err != nil {
			t.Errorf("valid %q: %v", text, err)
		}
	}
	for _, suffix := range []string{
		"\x00", "\x01", "\x1b", "\r", "\x7f", "\u0085", // controls
		"\u200b", "\u200c", "\u200d", "\u202e", "\u2066", "\u061c", "\ufff9", // formats
		"\u00ad", "\u034f", "\u115f", "\u1160", "\u17b4", "\u180b", "\u3164", "\uffa0", // other ignorables
		"\ufe0e", "\ufe0f", "\U000e0100", "\U000e0001", "\U000e0061", // selectors/tags
		"\u2800", "\U00013441", "\U00013442", "\U00016fe4", "\U0001d159", // blank letters
		"\u200d💻", "\ufe0f\ufe0f", string([]byte{0xff}),
	} {
		if err := Validate("界" + suffix); err == nil || !strings.Contains(err.Error(), "byte 4") {
			t.Errorf("suffix %q: %v, want refusal at byte 4", suffix, err)
		}
	}
	if err := Validate("❤️️"); err == nil || !strings.Contains(err.Error(), "byte 7") {
		t.Errorf("extra selector: %v", err)
	}
	if err := Validate("👩‍💻\u200d"); err == nil || !strings.Contains(err.Error(), "byte 12") {
		t.Errorf("extra joiner: %v", err)
	}
	if err := Validate(strings.Repeat("a", MaxBytes+1)); err == nil {
		t.Fatal("oversized text accepted")
	}
}

// The fixture is Unicode's published data, including minimally qualified
// forms. Only tag-based flags are excluded by this approval policy.
func TestPublishedEmojiSequences(t *testing.T) {
	f, err := os.Open("testdata/18.0/emoji-test.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		codes := strings.Fields(strings.SplitN(line, ";", 2)[0])
		var text strings.Builder
		tags := false
		for _, code := range codes {
			n, err := strconv.ParseInt(code, 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			r := rune(n)
			if r >= 0xe0000 && r <= 0xe007f {
				tags = true
			}
			text.WriteRune(r)
		}
		if tags {
			continue
		}
		if err := Validate(text.String()); err != nil {
			t.Fatalf("listed sequence %s: %v", strings.Join(codes, " "), err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count < 5000 {
		t.Fatalf("fixture exercised only %d sequences", count)
	}
}
