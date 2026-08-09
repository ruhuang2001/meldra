package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8PreservesBoundariesAndSuffixBudget(t *testing.T) {
	for _, character := range []string{"\u00e9", "\u754c", "\U0001F642"} {
		t.Run(character, func(t *testing.T) {
			const limit = 8
			got := truncateUTF8("abcde"+character+"extra", limit, "...")
			if !utf8.ValidString(got) || len(got) > limit {
				t.Fatalf("truncated value = %q, valid=%t, length=%d", got, utf8.ValidString(got), len(got))
			}
			if got != "abcde..." {
				t.Fatalf("truncated value = %q, want suffix within limit", got)
			}
		})
	}
}

func TestTruncateUTF8NormalizesInvalidInputAndSmallLimits(t *testing.T) {
	got := truncateUTF8("ok"+string([]byte{0xff})+strings.Repeat("x", 10), 6, "...")
	if !utf8.ValidString(got) || len(got) > 6 || !strings.HasSuffix(got, "...") {
		t.Fatalf("normalized truncation = %q", got)
	}
	if got := truncateUTF8(strings.Repeat("x", 8), 2, "[truncated]"); got != "[t" {
		t.Fatalf("small limit truncation = %q", got)
	}
}
