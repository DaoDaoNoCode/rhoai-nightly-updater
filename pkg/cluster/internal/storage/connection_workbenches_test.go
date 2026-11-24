package storage

import "testing"

// Each annotation value is handled like the webhook does—comma-separated
// pieces with namespace and name trimmed per segment. Segments that refer to
// this tool's Connection within the project are dropped; remaining text is
// unchanged.
func TestWithoutConnection(t *testing.T) {
	c := ConnectionSecretName
	cases := []struct {
		value, want string
		found       bool
	}{
		{"proj/" + c, "", true},
		{"proj/a,proj/" + c + ",proj/b", "proj/a,proj/b", true},
		{"proj/a, proj / " + c + " ,proj/b", "proj/a,proj/b", true},
		{"proj/" + c + ",proj/" + c, "", true},
		{"proj/a,,proj/b", "proj/a,proj/b", false},
		{"other/" + c + ",proj/a", "other/" + c + ",proj/a", false},
		{"proj/" + c + "-2", "proj/" + c + "-2", false},
		{"proj/x/" + c, "proj/x/" + c, false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, found := withoutConnection(tc.value, "proj")
		if got != tc.want || found != tc.found {
			t.Errorf("withoutConnection(%q) = %q, %v; want %q, %v", tc.value, got, found, tc.want, tc.found)
		}
	}
}
