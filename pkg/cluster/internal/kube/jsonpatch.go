package kube

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// JSONPatchOp is one operation of a JSON Patch (application/json-patch+json).
type JSONPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ApplyJSONPatch applies a JSON Patch (test, add, replace, remove) to a
// copy of doc, as the API server does: the whole patch or nothing.
func ApplyJSONPatch(doc map[string]any, ops []JSONPatchOp) (map[string]any, error) {
	var root any
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	for _, op := range ops {
		keys := PointerKeys(op.Path)
		var value any
		if op.Op != "remove" {
			if err := json.Unmarshal(op.Value, &value); err != nil {
				return nil, fmt.Errorf("%s %s: bad value: %w", op.Op, op.Path, err)
			}
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("%s of the whole document is not supported", op.Op)
		}
		parent, err := pointerTarget(root, keys[:len(keys)-1])
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", op.Op, op.Path, err)
		}
		last := keys[len(keys)-1]
		switch p := parent.(type) {
		case map[string]any:
			cur, exists := p[last]
			switch op.Op {
			case "test":
				if !exists || !JSONEqual(cur, value) {
					return nil, &JSONPatchTestError{path: op.Path}
				}
			case "add":
				p[last] = value
			case "replace":
				if !exists {
					return nil, fmt.Errorf("replace %s: the path does not exist", op.Path)
				}
				p[last] = value
			case "remove":
				if !exists {
					return nil, fmt.Errorf("remove %s: the path does not exist", op.Path)
				}
				delete(p, last)
			default:
				return nil, fmt.Errorf("unsupported op %q", op.Op)
			}
		case []any:
			if err := applyToArray(root, keys, p, last, op.Op, value); err != nil {
				return nil, fmt.Errorf("%s %s: %w", op.Op, op.Path, err)
			}
		default:
			return nil, fmt.Errorf("%s %s: the parent is not an object or array", op.Op, op.Path)
		}
	}
	out, _ := root.(map[string]any)
	return out, nil
}

// NormalizeJSON returns v as encoding/json decodes it (YAML numbers become
// float64), so values compare and patch alike.
func NormalizeJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// PointerKeys splits a JSON Pointer into its keys.
func PointerKeys(pointer string) []string {
	if pointer == "" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

// pointerTarget returns the value at keys.
func pointerTarget(root any, keys []string) (any, error) {
	cur := root
	for _, k := range keys {
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[k]
			if !ok {
				return nil, fmt.Errorf("the path does not exist")
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("index %s is out of range", k)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("the path does not exist")
		}
	}
	return cur, nil
}

// JSONEqual compares two decoded JSON values.
func JSONEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	var da, db any
	if json.Unmarshal(ja, &da) != nil || json.Unmarshal(jb, &db) != nil {
		return false
	}
	return reflect.DeepEqual(da, db)
}

// JSONPatchTestError is a failed "test" operation.
type JSONPatchTestError struct{ path string }

// applyToArray applies one operation to an element of array arr (at
// keys[:len(keys)-1] of root), replacing the array in its parent.
func applyToArray(root any, keys []string, arr []any, last, op string, value any) error {
	i := len(arr)
	if last != "-" {
		n, err := strconv.Atoi(last)
		if err != nil || n < 0 || n > len(arr) {
			return fmt.Errorf("index %s is out of range", last)
		}
		i = n
	}
	var next []any
	switch op {
	case "test":
		if i >= len(arr) || !JSONEqual(arr[i], value) {
			return &JSONPatchTestError{path: PointerOf(keys)}
		}
		return nil
	case "add":
		next = append(append(append([]any{}, arr[:i]...), value), arr[i:]...)
	case "replace":
		if i >= len(arr) {
			return fmt.Errorf("index %s is out of range", last)
		}
		next = append([]any{}, arr...)
		next[i] = value
	case "remove":
		if i >= len(arr) {
			return fmt.Errorf("index %s is out of range", last)
		}
		next = append(append([]any{}, arr[:i]...), arr[i+1:]...)
	default:
		return fmt.Errorf("unsupported op %q", op)
	}
	parentKeys := keys[:len(keys)-1]
	grand, err := pointerTarget(root, parentKeys[:len(parentKeys)-1])
	if err != nil {
		return err
	}
	switch g := grand.(type) {
	case map[string]any:
		g[parentKeys[len(parentKeys)-1]] = next
	case []any:
		n, _ := strconv.Atoi(parentKeys[len(parentKeys)-1])
		g[n] = next
	}
	return nil
}

// PointerOf returns the JSON Pointer (RFC 6901) of a path of keys.
func PointerOf(path []string) string {
	var b strings.Builder
	for _, k := range path {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

func (e *JSONPatchTestError) Error() string { return "testing value " + e.path + " failed" }
