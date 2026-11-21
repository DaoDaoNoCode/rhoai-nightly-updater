package kube

import (
	"encoding/json"
	"sort"
	"strings"
)

// RedactedText replaces a credential in a message.
const RedactedText = "[redacted]"

// RedactSecrets replaces every occurrence of each secret in msg, as it is
// and as a JSON string would carry it (an API error may quote the object it
// was sent), longest first, so a secret that contains another is replaced
// whole. Values shorter than 4 characters are left: they would blank out
// ordinary words, and no credential is that short.
func RedactSecrets(msg string, secrets ...string) string {
	var forms []string
	for _, s := range secrets {
		if len(s) < 4 {
			continue
		}
		forms = append(forms, s)
		if quoted, err := json.Marshal(s); err == nil {
			if q := string(quoted[1 : len(quoted)-1]); q != s {
				forms = append(forms, q)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, f := range forms {
		msg = strings.ReplaceAll(msg, f, RedactedText)
	}
	return msg
}
