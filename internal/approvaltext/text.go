// Package approvaltext validates text that a person is asked to approve.
package approvaltext

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:generate python3 generate.py

// MaxBytes is the largest text accepted for one approval, before storage.
const MaxBytes = 32 << 10

// Validate refuses text that is too large or contains characters that do
// not show. Positions are one-based UTF-8 byte offsets in the original text.
// Only listed Unicode emoji sequences may contain U+200D or U+FE0F.
func Validate(text string) error {
	if len(text) > MaxBytes {
		return fmt.Errorf("text exceeds %d bytes", MaxBytes)
	}
	for at := 0; at < len(text); {
		r, size := utf8.DecodeRuneInString(text[at:])
		if r == utf8.RuneError && size == 1 {
			return fmt.Errorf("invalid UTF-8 at byte %d", at+1)
		}
		matched := false
		for _, seq := range emojiSequences[r] {
			if strings.HasPrefix(text[at:], seq) {
				at += len(seq)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) || ignorable(r) || strings.ContainsRune("\u2800\U00013441\U00013442\U00016fe4\U0001d159", r) {
			return fmt.Errorf("character U+%04X does not show at byte %d", r, at+1)
		}
		at += size
	}
	return nil
}

func ignorable(r rune) bool {
	i := sort.Search(len(defaultIgnorable), func(i int) bool { return defaultIgnorable[i][1] >= r })
	return i < len(defaultIgnorable) && defaultIgnorable[i][0] <= r
}
