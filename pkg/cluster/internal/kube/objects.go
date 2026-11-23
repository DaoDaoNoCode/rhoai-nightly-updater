package kube

// Object helpers: metadata reads, UID-guarded deletes, waits for an object
// to be gone.

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

// ManagedByLabelKey=ManagedByLabelValue is the label on every object the
// updater creates; only labelled objects are listed for teardown.
const ManagedByLabelKey = "app.kubernetes.io/managed-by"

// ManagedByLabelValue: see ManagedByLabelKey.
const ManagedByLabelValue = "rhoai-nightly-updater"

// RecipeRunAnnotation marks an object an environment recipe run created,
// with the run's ID: set only when the run's step created the object, and
// required by that run's Clean up right before it deletes the object.
const RecipeRunAnnotation = "rhoai-nightly-updater/recipe-run"

// OfRun reports whether the object carries run's RecipeRunAnnotation.
func (m ObjectMeta) OfRun(run string) bool {
	return run != "" && m.Annotations[RecipeRunAnnotation] == run
}

// NotOfRunMessage refuses to delete what is not run's any more.
func NotOfRunMessage(what string) string {
	return what + " is not the one the recipe run created (it lacks the run's annotation: replaced, or set up by someone else), so it was not deleted."
}

// ObjectMeta is the subset of metadata the ownership checks read.
type ObjectMeta struct {
	Name              string               `json:"name"`
	Namespace         string               `json:"namespace"`
	UID               string               `json:"uid"`
	ResourceVersion   string               `json:"resourceVersion"`
	Generation        int64                `json:"generation"`
	CreationTimestamp string               `json:"creationTimestamp"`
	DeletionTimestamp string               `json:"deletionTimestamp"`
	Labels            map[string]string    `json:"labels"`
	Annotations       map[string]string    `json:"annotations"`
	Finalizers        []string             `json:"finalizers"`
	ManagedFields     []ManagedFieldsEntry `json:"managedFields"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"ownerReferences"`
}

// ManagedFieldsEntry is one metadata.managedFields entry, with every field of
// the Kubernetes type (metav1.ManagedFieldsEntry), so an entry read here
// can be written back unchanged.
type ManagedFieldsEntry struct {
	Manager    string `json:"manager,omitempty"`
	Operation  string `json:"operation,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	Time       string `json:"time,omitempty"`
	FieldsType string `json:"fieldsType,omitempty"`
	// FieldsV1 is the set of fields the manager owns, as a tree keyed by
	// "f:<field>", "k:<list key>", "v:<value>" or "." (the node itself).
	FieldsV1    map[string]json.RawMessage `json:"fieldsV1,omitempty"`
	Subresource string                     `json:"subresource,omitempty"`
}

func ParseObjectMeta(body []byte) (ObjectMeta, error) {
	var obj struct {
		Metadata ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ObjectMeta{}, err
	}
	return obj.Metadata, nil
}

// ReadObjectMeta reads the metadata of the object at path. found is false on
// 404; any other error is returned.
func ReadObjectMeta(c *Client, path string) (meta ObjectMeta, found bool, err error) {
	body, exists, err := c.GetOptional(path)
	if err != nil {
		return ObjectMeta{}, false, err
	}
	if !exists {
		return ObjectMeta{}, false, nil
	}
	meta, err = ParseObjectMeta(body)
	return meta, err == nil, err
}

// DeleteWithUID deletes path only if the object still has the given UID, so a
// check-then-delete never removes an object recreated in between.
func DeleteWithUID(c *Client, path, uid string) (int, error) {
	return DeleteWithUIDPropagation(c, path, uid, "Background")
}

// DeleteWithUIDPropagation is DeleteWithUID with a propagation policy.
// "Foreground" keeps the object (with a deletionTimestamp) until the garbage
// collector deleted its dependents that set blockOwnerDeletion (Kubernetes
// docs, Garbage Collection, "Foreground cascading deletion").
func DeleteWithUIDPropagation(c *Client, path, uid, policy string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"preconditions":     map[string]any{"uid": uid},
		"propagationPolicy": policy,
	})
	_, status, err := c.Do(http.MethodDelete, path, "application/json", body, nil)
	return status, err
}

// DeleteWithPreconditions is DeleteWithUID that also requires the object's
// resourceVersion: the delete fails with 409 when the object changed in
// any way since it was read (Kubernetes DeleteOptions.preconditions).
func DeleteWithPreconditions(c *Client, path, uid, resourceVersion string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"preconditions":     map[string]any{"uid": uid, "resourceVersion": resourceVersion},
		"propagationPolicy": "Background",
	})
	_, status, err := c.Do(http.MethodDelete, path, "application/json", body, nil)
	return status, err
}

// WaitForGone polls until gone reports true, an error occurs, or the timeout
// passes. Every poll runs under one context bounded by the timeout, so a call
// that starts just before the deadline cannot overrun it. It returns
// (false, nil) on timeout and (false, err) on any read error, including
// 401/403: an unreadable object is not "still being deleted".
func WaitForGone(c *Client, timeout, interval time.Duration, gone func(cc *Client) (bool, error)) (bool, error) {
	ctx, cancel := Clock().WithTimeout(c.Context(), timeout)
	defer cancel()
	cc := c.WithContext(ctx)
	for {
		done, err := gone(cc)
		if done && err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			// The caller's own context ended (client gone): report it. The
			// local deadline only means "not gone yet".
			if c.Context().Err() != nil {
				return false, c.Context().Err()
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_ = Clock().Sleep(ctx, interval) // the deadline is checked after the next poll
	}
}

// WaitForDeletion polls path until it returns 404. See WaitForGone.
func WaitForDeletion(c *Client, path string, timeout, interval time.Duration) (bool, error) {
	return WaitForGone(c, timeout, interval, func(cc *Client) (bool, error) {
		_, _, err := cc.Get(path)
		if IsK8sNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func (m ObjectMeta) HasToolLabel() bool {
	return m.Labels[ManagedByLabelKey] == ManagedByLabelValue
}

func (m ObjectMeta) Terminating() bool {
	return m.DeletionTimestamp != ""
}

// OwnsFieldPath reports whether one of managers owns the field at path, a
// chain of fieldsV1 keys such as "f:spec", "f:objectStorage", through the
// object itself (not a subresource). The API server gives a field to
// whoever last set it to a new value, so a field the tool still owns is one
// the tool wrote and nobody has changed since: proof of who made the
// object's content that, unlike an entry's time (the manager's last write),
// does not break when the tool updates the object again.
func (m ObjectMeta) OwnsFieldPath(managers []string, path ...string) bool {
	for _, e := range m.ManagedFields {
		if e.Subresource != "" || !slices.Contains(managers, e.Manager) || len(path) == 0 {
			continue
		}
		raw, ok := e.FieldsV1[path[0]]
		for _, key := range path[1:] {
			if !ok {
				break
			}
			var node map[string]json.RawMessage
			if json.Unmarshal(raw, &node) != nil {
				ok = false
				break
			}
			raw, ok = node[key]
		}
		if ok {
			return true
		}
	}
	return false
}

// HasManager reports whether one of managers wrote the object itself at
// any time (any operation, not a subresource).
func (m ObjectMeta) HasManager(managers []string) bool {
	for _, e := range m.ManagedFields {
		if e.Subresource == "" && slices.Contains(managers, e.Manager) {
			return true
		}
	}
	return false
}

// OwnsField reports whether manager owns the top-level field (a fieldsV1
// key such as "f:spec") through the object itself, not a subresource.
func (m ObjectMeta) OwnsField(manager, field string) bool {
	for _, f := range m.ManagedFields {
		if _, owns := f.FieldsV1[field]; owns && f.Manager == manager && f.Subresource == "" {
			return true
		}
	}
	return false
}

func (m ObjectMeta) OwnedBy(uid string) bool {
	for _, o := range m.OwnerReferences {
		if o.UID == uid {
			return true
		}
	}
	return false
}

// DeleteExact deletes an object only if it still has the UID and
// resourceVersion that were inspected (DeleteOptions preconditions).
func DeleteExact(c *Client, path, uid, resourceVersion string) error {
	opts := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}
	pre := map[string]string{}
	if uid != "" {
		pre["uid"] = uid
	}
	if resourceVersion != "" {
		pre["resourceVersion"] = resourceVersion
	}
	if len(pre) > 0 {
		opts["preconditions"] = pre
	}
	body, _ := json.Marshal(opts)
	_, _, err := c.Do(http.MethodDelete, path, "application/json", body, nil)
	return err
}
