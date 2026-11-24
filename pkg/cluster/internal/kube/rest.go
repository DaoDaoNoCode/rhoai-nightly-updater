package kube

// REST helpers: the request patterns most call sites repeat, on top of the
// Client request methods (client.go). Each returns exactly the errors its
// call sites saw when they made the request themselves.
//
// The RBAC scanner (rbac_coverage_test.go) treats each helper as the
// request method it wraps (helperMethods) and skips this file, as it skips
// client.go: the request is attributed to the call site's path.

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"
)

// IsK8sNotFound reports whether err is the API server's 404: the object (or
// the resource type) does not exist. Not the MLflow 404 (isNotFound).
func IsK8sNotFound(err error) bool { return IsK8sError(err, http.StatusNotFound) }

// IsK8sConflict reports whether err is the API server's 409: the object
// already exists (a create) or changed since it was read (an update).
func IsK8sConflict(err error) bool { return IsK8sError(err, http.StatusConflict) }

// GetOptional reads path, where a missing object (404) is no error: found
// is then false, with no body. Any other error is Get's, with its body.
func (c *Client) GetOptional(path string) (body []byte, found bool, err error) {
	body, _, err = c.Get(path)
	if err != nil {
		if IsK8sNotFound(err) {
			return nil, false, nil
		}
		return body, false, err
	}
	return body, true, nil
}

// decodeError is a response body that did not decode into the caller's
// value. Its message is the JSON error's own, so a caller wraps it as it
// wrapped json.Unmarshal's error; it unwraps to that error.
type decodeError struct{ err error }

func (e *decodeError) Error() string { return e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

// IsDecodeError reports whether err is a decode error of GetInto,
// GetIntoOptional or ListInto, not the request's error.
func IsDecodeError(err error) bool {
	_, ok := errors.AsType[*decodeError](err)
	return ok
}

// GetInto reads path and decodes the body into out (a pointer), as
// json.Unmarshal(body, out). A request error is Get's; a decode error is a
// *decodeError.
func (c *Client) GetInto(path string, out any) error {
	body, _, err := c.Get(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &decodeError{err}
	}
	return nil
}

// GetIntoOptional is GetInto where a missing object (404) is no error:
// found is then false and out is left as it was.
func (c *Client) GetIntoOptional(path string, out any) (found bool, err error) {
	body, found, err := c.GetOptional(path)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return true, &decodeError{err}
	}
	return true, nil
}

// ListInto reads the list at path and stores its items in items (a pointer
// to a slice). It decodes the body into struct{ Items []T `json:"items"` },
// the unnamed list struct the call sites declared, so a decode error (a
// *decodeError) has the message json.Unmarshal gave them.
func (c *Client) ListInto(path string, items any) error {
	_, err := c.ListIntoAt(path, items)
	return err
}

// ListIntoAt is ListInto that also returns the API server's time when it
// answered: its Date header (RFC 9110 section 6.6.1, to the second), or the
// zero time when the response carries none that parses. A caller that
// compares an object's timestamps with "now" uses it, so the gap between
// this tool's clock and the cluster's does not count.
func (c *Client) ListIntoAt(path string, items any) (time.Time, error) {
	out := reflect.ValueOf(items)
	if out.Kind() != reflect.Pointer || out.Elem().Kind() != reflect.Slice {
		panic("ListInto: items must be a pointer to a slice")
	}
	list := reflect.New(reflect.StructOf([]reflect.StructField{{
		Name: "Items",
		Type: out.Elem().Type(),
		Tag:  `json:"items"`,
	}}))
	body, header, _, err := c.doWithHeader(http.MethodGet, path, "", nil, nil)
	if err != nil {
		return time.Time{}, err
	}
	if err := json.Unmarshal(body, list.Interface()); err != nil {
		return time.Time{}, &decodeError{err}
	}
	out.Elem().Set(list.Elem().Field(0))
	at, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return time.Time{}, nil
	}
	return at, nil
}

// PatchJSON sends patch, marshalled to JSON, as a merge patch (Patch). A
// marshal error is json.Marshal's, before any request; any other error is
// Patch's.
func (c *Client) PatchJSON(path string, patch any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, _, err = c.Patch(path, data)
	return err
}

// DeleteIfExists deletes the object at path. An object that is already gone
// (404) is no error: existed is then false. Other errors are Delete's.
func (c *Client) DeleteIfExists(path string) (existed bool, err error) {
	if _, err := c.Delete(path); err != nil {
		if IsK8sNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
