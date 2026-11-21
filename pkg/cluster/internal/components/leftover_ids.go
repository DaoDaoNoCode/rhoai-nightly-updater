package components

import "strings"

// The leftovers check's name and the prefix of its problem IDs, which the
// apply-failure and DataScienceCluster checks name too.
const (
	LeftoversCheckName = "Left over from an earlier release"
	LeftoverIDPrefix   = "leftover-"
)

func LeftoverFieldsID(kind, name string) string {
	return LeftoverIDPrefix + "fields-" + strings.ToLower(kind+"-"+name)
}

func LeftoverRecreateID(kind, namespace, name string) string {
	return LeftoverIDPrefix + "recreate-" + strings.ToLower(kind+"-"+namespace+"-"+name)
}
