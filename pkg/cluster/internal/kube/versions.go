package kube

import (
	"strconv"
	"strings"
)

// CompareVersions orders dotted numeric versions ("3.6.1"): negative, zero
// or positive as a is lower, equal or higher. A missing or non-numeric part
// counts as 0; with equal parts, more parts is higher.
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return len(pa) - len(pb)
}
