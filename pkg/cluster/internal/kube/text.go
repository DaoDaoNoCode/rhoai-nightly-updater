package kube

// Text helpers for the messages users read.

import (
	"net/url"
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

// snippetURLPattern is a plain https URL: a host name, an optional port and
// a path of unreserved characters and percent escapes. No query, fragment,
// user info, quote, space, $, backtick, ;, &, | or other character a shell
// treats specially.
var snippetURLPattern = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$`)

// SnippetURL reports whether s, an address read from the cluster (a
// model's status.url, a pipeline server's API address, the MaaS address),
// may be shown in commands users paste into a shell. Whoever can write the
// object's status (or the cluster's ingress domain) chooses it; the UI
// shell-quotes it as well.
func SnippetURL(s string) bool {
	if !snippetURLPattern.MatchString(s) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}
