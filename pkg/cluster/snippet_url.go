package cluster

import (
	"net/url"
	"regexp"
)

// snippetURLPattern is a plain https URL: a host name, an optional port and
// a path of unreserved characters and percent escapes. No query, fragment,
// user info, quote, space, $, backtick, ;, &, | or other character a shell
// treats specially.
var snippetURLPattern = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$`)

// snippetURL reports whether s, an address read from the cluster (a
// model's status.url, a pipeline server's API address, the MaaS address),
// may be shown in commands users paste into a shell. Whoever can write the
// object's status (or the cluster's ingress domain) chooses it; the UI
// shell-quotes it as well.
func snippetURL(s string) bool {
	if !snippetURLPattern.MatchString(s) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

// unsafeAddressWarning says why an address is not shown.
func unsafeAddressWarning(what string) string {
	return "The " + what + " the cluster reports is not a plain https address, so this tool does not show it or put it in commands."
}
