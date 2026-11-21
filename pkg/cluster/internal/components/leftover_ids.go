package components

import "strings"

// The leftovers check's name and the prefix of its problem IDs, which the
// apply-failure and DataScienceCluster checks name too.
const (
	LeftoversCheckName = "Left over from an earlier release"
	leftoverIDPrefix   = "leftover-"
)

func LeftoverFieldsID(kind, name string) string {
	return leftoverIDPrefix + "fields-" + strings.ToLower(kind+"-"+name)
}

func LeftoverRecreateID(kind, namespace, name string) string {
	return leftoverIDPrefix + "recreate-" + strings.ToLower(kind+"-"+namespace+"-"+name)
}
