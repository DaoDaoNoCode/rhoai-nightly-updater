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

// servedNamePattern is a name a model may serve under that is shown and put
// in commands: the characters of Kubernetes names and Hugging Face ids
// (org/model, model:tag), nothing a shell or JSON treats specially.
var servedNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/:-]{1,253}$`)

// safeServedName reports whether s, a model's spec.model.name, may be
// shown, put in the commands users paste into a shell, and sent by the
// call check. KServe and vLLM accept any string there, and anyone who can
// edit the model's project chooses it (on any model of a catalog name, not
// only this tool's); the UI also builds request bodies as JSON and
// shell-quotes them.
func safeServedName(s string) bool {
	return servedNamePattern.MatchString(s)
}

// unsafeServedNameWarning says why a model's served name is not shown.
const unsafeServedNameWarning = "The name the model serves (spec.model.name) has characters other than letters, digits and . _ / : -, so this tool does not show it, put it in commands or call the model with it."

// unsafeAddressWarning says why an address is not shown.
func unsafeAddressWarning(what string) string {
	return "The " + what + " the cluster reports is not a plain https address, so this tool does not show it or put it in commands."
}
