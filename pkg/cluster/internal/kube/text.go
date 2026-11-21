package kube

// Text helpers for the messages users read.

import (
	"strings"
)

// FirstNonEmpty returns the first value that is not empty, or "".
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// TruncateText trims s and cuts it to its first n characters (runes, so a
// multi-byte character is never split), marking the cut with an ellipsis.
func TruncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// NonEmpty returns s, or fallback when s is empty.
func NonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
