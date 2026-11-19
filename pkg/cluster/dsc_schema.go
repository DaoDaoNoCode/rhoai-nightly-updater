package cluster

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The DataScienceCluster as its installed CRD defines it. Everything the
// Components page offers (which components and sub-components exist, the
// managementState values each accepts, which fields must keep their value)
// is read from the CRD the cluster serves, never from a list in this code:
// releases add, move and drop components often (rhods-operator 3.6 moved
// the dashboard under dashboard.standard and renamed modelregistry to
// aiHub).

const dscCRDPath = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io"

// dscCRDVersion is one version of the DataScienceCluster CRD.
type dscCRDVersion struct {
	Name       string
	Served     bool
	Storage    bool
	Deprecated bool
	// Spec is openAPIV3Schema.properties.spec.
	Spec map[string]any
}

// dscCRD is the installed DataScienceCluster CRD.
type dscCRD struct {
	Versions []dscCRDVersion
}

// readDSCCRD reads the installed DataScienceCluster CRD (get on the CRD
// by name, which both profiles grant).
func readDSCCRD(c *Client) (*dscCRD, error) {
	body, _, err := c.get(dscCRDPath)
	if err != nil {
		return nil, err
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name       string `json:"name"`
				Served     bool   `json:"served"`
				Storage    bool   `json:"storage"`
				Deprecated bool   `json:"deprecated"`
				Schema     struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &crd); err != nil {
		return nil, fmt.Errorf("parse the DataScienceCluster CRD: %w", err)
	}
	out := &dscCRD{}
	for _, v := range crd.Spec.Versions {
		props, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]any)
		spec, _ := props["spec"].(map[string]any)
		out.Versions = append(out.Versions, dscCRDVersion{Name: v.Name, Served: v.Served, Storage: v.Storage, Deprecated: v.Deprecated, Spec: spec})
	}
	return out, nil
}

// storage returns the storage version's name ("" when none is marked).
func (d *dscCRD) storage() string {
	for _, v := range d.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// version returns the served version name, or nil.
func (d *dscCRD) version(name string) *dscCRDVersion {
	for i := range d.Versions {
		if d.Versions[i].Name == name && d.Versions[i].Served {
			return &d.Versions[i]
		}
	}
	return nil
}

// schemaProps returns an object schema's properties.
func schemaProps(schema map[string]any) map[string]any {
	p, _ := schema["properties"].(map[string]any)
	return p
}

// schemaChild returns the schema of key in an object schema, or nil.
func schemaChild(schema map[string]any, key string) map[string]any {
	child, _ := schemaProps(schema)[key].(map[string]any)
	return child
}

// dscToggle is an object under spec.components the CRD defines that has a
// managementState (Toggle), or that holds such objects (a group).
type dscToggle struct {
	Path          []string // under spec.components: ["kserve", "nim"]
	Toggle        bool
	Allowed       []string
	SchemaDefault string
}

func (t dscToggle) key() string { return strings.Join(t.Path, ".") }

// managementPattern is the pattern form of an enum ("^(Managed|Removed)$").
var managementPattern = regexp.MustCompile(`^\^?\(?([A-Za-z]+(?:\|[A-Za-z]+)*)\)?\$?$`)

// allowedStates returns the values a managementState schema accepts: its
// enum, or, only without one, the alternatives of a simple pattern.
func allowedStates(schema map[string]any) []string {
	var out []string
	if enum, ok := schema["enum"].([]any); ok {
		for _, v := range enum {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if pattern, _ := schema["pattern"].(string); pattern != "" {
		if m := managementPattern.FindStringSubmatch(pattern); m != nil {
			return strings.Split(m[1], "|")
		}
	}
	return nil
}

// dscToggleTree returns every object of spec.components that has a
// managementState or holds one, at any depth, parents before their
// children, siblings by name.
func dscToggleTree(spec map[string]any) []dscToggle {
	components := schemaChild(spec, "components")
	var walk func(schema map[string]any, path []string) []dscToggle
	walk = func(schema map[string]any, path []string) []dscToggle {
		props := schemaProps(schema)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := strings.ToLower(keys[i]), strings.ToLower(keys[j])
			if a != b {
				return a < b
			}
			return keys[i] < keys[j]
		})
		var out []dscToggle
		for _, k := range keys {
			child, _ := props[k].(map[string]any)
			if child == nil || k == "managementState" || schemaProps(child) == nil {
				continue
			}
			p := append(append([]string{}, path...), k)
			node := dscToggle{Path: p}
			if ms := schemaChild(child, "managementState"); ms != nil {
				node.Toggle = true
				node.Allowed = allowedStates(ms)
				node.SchemaDefault, _ = ms["default"].(string)
			}
			sub := walk(child, p)
			if node.Toggle || len(sub) > 0 {
				out = append(out, node)
				out = append(out, sub...)
			}
		}
		return out
	}
	if components == nil {
		return nil
	}
	return walk(components, nil)
}

// sameFieldRule finds "self.<f> == oldSelf.<f>" in a CEL rule.
var sameFieldRule = regexp.MustCompile(`self\.([A-Za-z0-9_]+)\s*==\s*oldSelf\.([A-Za-z0-9_]+)`)

// dscProtectedFields returns the fields under spec.components (dotted
// paths) whose value the CRD's validation rules (x-kubernetes-validations)
// keep: a field with the rule "self == oldSelf" (workbenches.workbenchNamespace,
// "WorkbenchNamespace is immutable"), and a field an object's rule compares
// with its old value (aiHub.instancesNamespace, "InstancesNamespace is
// immutable when AI Hub is Managed"). A reset keeps their value whenever the
// DSC has one: they name namespaces that hold users' data.
func dscProtectedFields(spec map[string]any) map[string]bool {
	out := map[string]bool{}
	var walk func(schema map[string]any, path []string)
	walk = func(schema map[string]any, path []string) {
		if rules, ok := schema["x-kubernetes-validations"].([]any); ok {
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				rule, _ := rm["rule"].(string)
				if strings.Join(strings.Fields(rule), " ") == "self == oldSelf" && len(path) > 0 {
					out[strings.Join(path, ".")] = true
				}
				for _, m := range sameFieldRule.FindAllStringSubmatch(rule, -1) {
					if m[1] == m[2] {
						out[strings.Join(append(append([]string{}, path...), m[1]), ".")] = true
					}
				}
			}
		}
		for k, v := range schemaProps(schema) {
			if child, ok := v.(map[string]any); ok {
				walk(child, append(append([]string{}, path...), k))
			}
		}
	}
	if components := schemaChild(spec, "components"); components != nil {
		walk(components, nil)
	}
	return out
}

// applySchemaDefaults fills in x, in place, what the API server's
// defaulting adds to an object of the installed CRD (Kubernetes structural
// schema defaulting, apiextensions-apiserver
// pkg/apiserver/schema/defaulting/algorithm.go Default): in every object,
// a property that is missing (or null and not nullable) gets its schema
// default; then every property is defaulted below, a default just set
// included; array items and additionalProperties alike. The API server
// does this on every create, update and read, so a field it defaulted
// (no field manager owns it) comes back by itself in a new object.
func applySchemaDefaults(x any, schema map[string]any) {
	if schema == nil {
		return
	}
	switch x := x.(type) {
	case map[string]any:
		props := schemaProps(schema)
		for k, p := range props {
			ps, _ := p.(map[string]any)
			def, ok := ps["default"]
			if !ok {
				continue
			}
			if cur, found := x[k]; !found || (cur == nil && ps["nullable"] != true) {
				x[k] = normalizeJSON(def)
			}
		}
		additional, _ := schema["additionalProperties"].(map[string]any)
		for k, v := range x {
			if ps, ok := props[k].(map[string]any); ok {
				applySchemaDefaults(v, ps)
			} else if additional != nil {
				applySchemaDefaults(v, additional)
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return
		}
		for i := range x {
			if def, ok := items["default"]; ok && x[i] == nil && items["nullable"] != true {
				x[i] = normalizeJSON(def)
			}
			applySchemaDefaults(x[i], items)
		}
	}
}

// withSchemaDefaults returns a copy of obj with the schema's defaults
// filled in (applySchemaDefaults); an empty object for nil.
func withSchemaDefaults(obj map[string]any, schema map[string]any) map[string]any {
	out, _ := normalizeJSON(obj).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	applySchemaDefaults(out, schema)
	return out
}

// defaultedBack reports whether removing a field that holds value changes
// nothing: its schema has a default, and the API server's defaulting puts
// exactly value back (the default with the defaults below it). Removing
// such a field is no change, so a reset neither sends nor lists it.
func defaultedBack(schema map[string]any, value any) bool {
	def, ok := schema["default"]
	if !ok {
		return false
	}
	back, now := normalizeJSON(def), normalizeJSON(value)
	applySchemaDefaults(back, schema)
	applySchemaDefaults(now, schema)
	return jsonEqual(back, now)
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
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

// lookupPath returns the value at path in a decoded JSON object.
func lookupPath(obj map[string]any, path []string) (any, bool) {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// lookupState returns the managementState of the object at path.
func lookupState(components map[string]any, path []string) (string, bool) {
	v, ok := lookupPath(components, append(append([]string{}, path...), "managementState"))
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// --- JSON Patch (RFC 6902) -------------------------------------------------

// jsonPatchOp is one operation of a JSON Patch (application/json-patch+json).
type jsonPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// pointerOf returns the JSON Pointer (RFC 6901) of a path of keys.
func pointerOf(path []string) string {
	var b strings.Builder
	for _, k := range path {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// pointerKeys splits a JSON Pointer into its keys.
func pointerKeys(pointer string) []string {
	if pointer == "" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

func patchOp(op string, path []string, value any) jsonPatchOp {
	o := jsonPatchOp{Op: op, Path: pointerOf(path)}
	if op != "remove" {
		raw, _ := json.Marshal(value)
		o.Value = raw
	}
	return o
}

// dscAssign sets the value at Path (keys from the DSC's root, such as
// ["spec", "components", "kserve", "managementState"]).
type dscAssign struct {
	Path  []string
	Value any
}

// buildJSONPatch returns a JSON Patch that makes exactly these changes to
// obj and fails as a whole when anything it changes differs from what obj
// holds now: each replaced or removed value is tested first, and a value
// added where nothing is gets a test of the nearest object that exists (a
// concurrent change anywhere in it fails the patch rather than being
// overwritten). Missing objects on the way are added with the value. All
// tests come first, so they see the object as read.
func buildJSONPatch(obj map[string]any, sets []dscAssign, removes [][]string) ([]jsonPatchOp, error) {
	var tests, changes []jsonPatchOp
	tested := map[string]bool{}
	test := func(path []string, value any) {
		p := pointerOf(path)
		if !tested[p] {
			tested[p] = true
			tests = append(tests, patchOp("test", path, value))
		}
	}
	for _, r := range removes {
		v, ok := lookupPath(obj, r)
		if !ok {
			return nil, fmt.Errorf("%s is not in the DataScienceCluster", strings.Join(r, "."))
		}
		test(r, v)
		changes = append(changes, patchOp("remove", r, nil))
	}
	// Sets whose path is missing are grouped under the first missing key.
	type insertion struct {
		root  []string
		value map[string]any
		leaf  any
		whole bool // the root itself is the value
	}
	var inserts []*insertion
	byRoot := map[string]*insertion{}
	for _, s := range sets {
		if len(s.Path) == 0 {
			return nil, fmt.Errorf("empty path")
		}
		if cur, ok := lookupPath(obj, s.Path); ok {
			if jsonEqual(cur, s.Value) {
				continue
			}
			test(s.Path, cur)
			changes = append(changes, patchOp("replace", s.Path, s.Value))
			continue
		}
		k := 0
		for k < len(s.Path)-1 {
			v, ok := lookupPath(obj, s.Path[:k+1])
			if !ok {
				break
			}
			if _, isMap := v.(map[string]any); !isMap {
				return nil, fmt.Errorf("%s is not an object", strings.Join(s.Path[:k+1], "."))
			}
			k++
		}
		root := s.Path[:k+1]
		parent, _ := lookupPath(obj, s.Path[:k])
		if len(s.Path[:k]) == 0 {
			parent = obj
		}
		test(s.Path[:k], parent)
		key := pointerOf(root)
		in := byRoot[key]
		if in == nil {
			in = &insertion{root: root, value: map[string]any{}}
			byRoot[key] = in
			inserts = append(inserts, in)
		}
		rest := s.Path[k+1:]
		if len(rest) == 0 {
			in.whole, in.leaf = true, s.Value
			continue
		}
		m := in.value
		for _, key := range rest[:len(rest)-1] {
			next, ok := m[key].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[key] = next
			}
			m = next
		}
		m[rest[len(rest)-1]] = s.Value
	}
	for _, in := range inserts {
		if in.whole {
			if len(in.value) > 0 {
				return nil, fmt.Errorf("conflicting changes at %s", strings.Join(in.root, "."))
			}
			changes = append(changes, patchOp("add", in.root, in.leaf))
			continue
		}
		changes = append(changes, patchOp("add", in.root, in.value))
	}
	return append(tests, changes...), nil
}

// errJSONPatchTest is a failed "test" operation.
type errJSONPatchTest struct{ path string }

func (e *errJSONPatchTest) Error() string { return "testing value " + e.path + " failed" }

// applyJSONPatch applies a JSON Patch (test, add, replace, remove) to a
// copy of doc, as the API server does: the whole patch or nothing.
func applyJSONPatch(doc map[string]any, ops []jsonPatchOp) (map[string]any, error) {
	var root any
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	for _, op := range ops {
		keys := pointerKeys(op.Path)
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
				if !exists || !jsonEqual(cur, value) {
					return nil, &errJSONPatchTest{path: op.Path}
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
		if i >= len(arr) || !jsonEqual(arr[i], value) {
			return &errJSONPatchTest{path: pointerOf(keys)}
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
