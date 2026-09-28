package seed

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// MaxLen is a character limit (varchar(n) on Postgres and MySQL). Counting
// bytes cut Cyrillic and CJK text to about half of what the column holds.
func TestClampTextCountsCharacters(t *testing.T) {
	s := strings.Repeat("ж", 10) // 20 bytes, 10 characters
	if got := clampText(s, 10).(string); got != s {
		t.Fatalf("clampText cut a 10-character value to %q under a 10-character limit", got)
	}
	got := clampText(s+"ж", 10).(string)
	if utf8.RuneCountInString(got) != 10 || !utf8.ValidString(got) {
		t.Fatalf("clampText = %q (%d chars), want 10 valid characters", got, utf8.RuneCountInString(got))
	}
}
