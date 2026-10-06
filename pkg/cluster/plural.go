package cluster

import "fmt"

// countNoun formats a count with the singular or plural noun ("1 pod",
// "3 pods"), for messages users read.
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// verb picks the verb form that agrees with a count ("uses"/"use").
func verb(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
