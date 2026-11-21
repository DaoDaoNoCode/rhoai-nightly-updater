package cluster

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsK8sNotFoundAndConflict(t *testing.T) {
	cases := []struct {
		err                 error
		notFound, conflicts bool
	}{
		{&K8sError{Status: 404}, true, false},
		{fmt.Errorf("read: %w", &K8sError{Status: 404}), true, false},
		{&K8sError{Status: 409}, false, true},
		{fmt.Errorf("create: %w", &K8sError{Status: 409}), false, true},
		{&K8sError{Status: 403}, false, false},
		{&sampleAPIError{Status: 404}, false, false},
		{errors.New("404"), false, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		if got := isK8sNotFound(tc.err); got != tc.notFound {
			t.Errorf("isK8sNotFound(%v) = %v", tc.err, got)
		}
		if got := isK8sConflict(tc.err); got != tc.conflicts {
			t.Errorf("isK8sConflict(%v) = %v", tc.err, got)
		}
	}
}
