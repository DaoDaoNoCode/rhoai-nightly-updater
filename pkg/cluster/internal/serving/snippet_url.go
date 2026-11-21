package serving

import (
	"regexp"
)

// servedNamePattern is a name a model may serve under that is shown and put
// in commands: the characters of Kubernetes names and Hugging Face ids
// (org/model, model:tag), nothing a shell or JSON treats specially.
var servedNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/:-]{1,253}$`)

// SafeServedName reports whether s, a model's spec.model.name, may be
// shown, put in the commands users paste into a shell, and sent by the
// call check. KServe and vLLM accept any string there, and anyone who can
// edit the model's project chooses it (on any model of a catalog name, not
// only this tool's); the UI also builds request bodies as JSON and
// shell-quotes them.
func SafeServedName(s string) bool {
	return servedNamePattern.MatchString(s)
}

// unsafeServedNameWarning says why a model's served name is not shown.
const unsafeServedNameWarning = "The name the model serves (spec.model.name) has characters other than letters, digits and . _ / : -, so this tool does not show it, put it in commands or call the model with it."
