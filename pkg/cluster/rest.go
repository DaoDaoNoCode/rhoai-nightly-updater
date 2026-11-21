package cluster

// REST helpers: the request patterns most call sites repeat, on top of the
// Client request methods (client.go). Each returns exactly the errors its
// call sites saw when they made the request themselves.
//
// The RBAC scanner (rbac_coverage_test.go) treats each helper as the
// request method it wraps (helperMethods) and skips this file, as it skips
// client.go: the request is attributed to the call site's path.

import "net/http"

// isK8sNotFound reports whether err is the API server's 404: the object (or
// the resource type) does not exist. Not the MLflow 404 (isNotFound).
func isK8sNotFound(err error) bool { return IsK8sError(err, http.StatusNotFound) }

// isK8sConflict reports whether err is the API server's 409: the object
// already exists (a create) or changed since it was read (an update).
func isK8sConflict(err error) bool { return IsK8sError(err, http.StatusConflict) }

// deleteIfExists deletes the object at path. An object that is already gone
// (404) is no error: existed is then false. Other errors are delete's.
func (c *Client) deleteIfExists(path string) (existed bool, err error) {
	if _, err := c.delete(path); err != nil {
		if isK8sNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
