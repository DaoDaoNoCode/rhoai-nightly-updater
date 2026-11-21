package kube

// Text helpers for the messages users read.

import (
	"regexp"
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

// ShellQuote quotes s for a POSIX shell unless it only has characters that
// need no quoting.
func ShellQuote(s string) string {
	if s != "" && safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9._/:=@%+,-]+$`)

// ShellCommand joins words into a command line, quoting each.
func ShellCommand(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = ShellQuote(w)
	}
	return strings.Join(quoted, " ")
}

// SentenceStart returns s with its first letter in upper case.
func SentenceStart(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
