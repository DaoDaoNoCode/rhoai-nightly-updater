package components

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The DataScienceCluster as its installed CRD defines it. Everything the
// Components page offers (which components and sub-components exist, the
// managementState values each accepts, which fields must keep their value)
// is read from the CRD the cluster serves, never from a list in this code:
// releases add, move and drop components often (rhods-operator 3.6 moved
// the dashboard under dashboard.standard and renamed modelregistry to
// aiHub).

const DSCCRDPath = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io"

// dscCRDVersion is one version of the DataScienceCluster CRD.
type dscCRDVersion struct {
	Name       string
	Served     bool
	Storage    bool
	Deprecated bool
	// Spec is openAPIV3Schema.properties.spec.
	Spec map[string]any
}

// DSCCRD is the installed DataScienceCluster CRD.
type DSCCRD struct {
	Versions []dscCRDVersion
}

// ReadDSCCRD reads the installed DataScienceCluster CRD (get on the CRD
// by name, which both profiles grant).
func ReadDSCCRD(c *kube.Client) (*DSCCRD, error) {
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
	if err := c.GetInto(DSCCRDPath, &crd); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the DataScienceCluster CRD: %w", err)
		}
		return nil, err
	}
	out := &DSCCRD{}
	for _, v := range crd.Spec.Versions {
		props, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]any)
		spec, _ := props["spec"].(map[string]any)
		out.Versions = append(out.Versions, dscCRDVersion{Name: v.Name, Served: v.Served, Storage: v.Storage, Deprecated: v.Deprecated, Spec: spec})
	}
	return out, nil
}

// Storage returns the storage version's name ("" when none is marked).
func (d *DSCCRD) Storage() string {
	for _, v := range d.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// Version returns the served version name, or nil.
func (d *DSCCRD) Version(name string) *dscCRDVersion {
	for i := range d.Versions {
		if d.Versions[i].Name == name && d.Versions[i].Served {
			return &d.Versions[i]
		}
	}
	return nil
}

// SchemaProps returns an object schema's properties.
func SchemaProps(schema map[string]any) map[string]any {
	p, _ := schema["properties"].(map[string]any)
	return p
}

// SchemaChild returns the schema of key in an object schema, or nil.
func SchemaChild(schema map[string]any, key string) map[string]any {
	child, _ := SchemaProps(schema)[key].(map[string]any)
	return child
}

// DSCToggle is an object under spec.components the CRD defines that has a
// managementState (Toggle), or that holds such objects (a group).
type DSCToggle struct {
	Path          []string // under spec.components: ["kserve", "nim"]
	Toggle        bool
	Allowed       []string
	SchemaDefault string
}

func (t DSCToggle) Key() string { return strings.Join(t.Path, ".") }

// managementPattern is the pattern form of an enum ("^(Managed|Removed)$").
var managementPattern = regexp.MustCompile(`^\^?\(?([A-Za-z]+(?:\|[A-Za-z]+)*)\)?\$?$`)

// AllowedStates returns the values a managementState schema accepts: its
// enum, or, only without one, the alternatives of a simple pattern.
func AllowedStates(schema map[string]any) []string {
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

// DSCToggleTree returns every object of spec.components that has a
// managementState or holds one, at any depth, parents before their
// children, siblings by name.
func DSCToggleTree(spec map[string]any) []DSCToggle {
	components := SchemaChild(spec, "components")
	var walk func(schema map[string]any, path []string) []DSCToggle
	walk = func(schema map[string]any, path []string) []DSCToggle {
		props := SchemaProps(schema)
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
		var out []DSCToggle
		for _, k := range keys {
			child, _ := props[k].(map[string]any)
			if child == nil || k == "managementState" || SchemaProps(child) == nil {
				continue
			}
			p := append(append([]string{}, path...), k)
			node := DSCToggle{Path: p}
			if ms := SchemaChild(child, "managementState"); ms != nil {
				node.Toggle = true
				node.Allowed = AllowedStates(ms)
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

// DSCProtectedFields returns the fields under spec.components (dotted
// paths) whose value the CRD's validation rules (x-kubernetes-validations)
// keep: a field with the rule "self == oldSelf" (workbenches.workbenchNamespace,
// "WorkbenchNamespace is immutable"), and a field an object's rule compares
// with its old value (aiHub.instancesNamespace, "InstancesNamespace is
// immutable when AI Hub is Managed"). A reset keeps their value whenever the
// DSC has one: they name namespaces that hold users' data.
func DSCProtectedFields(spec map[string]any) map[string]bool {
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
		for k, v := range SchemaProps(schema) {
			if child, ok := v.(map[string]any); ok {
				walk(child, append(append([]string{}, path...), k))
			}
		}
	}
	if components := SchemaChild(spec, "components"); components != nil {
		walk(components, nil)
	}
	return out
}

// ApplySchemaDefaults fills in x, in place, what the API server's
// defaulting adds to an object of the installed CRD (Kubernetes structural
// schema defaulting, apiextensions-apiserver
// pkg/apiserver/schema/defaulting/algorithm.go Default): in every object,
// a property that is missing (or null and not nullable) gets its schema
// default; then every property is defaulted below, a default just set
// included; array items and additionalProperties alike: a null value of an
// additionalProperties entry whose schema is not nullable gets that
// schema's default too (kubernetes v1.34 algorithm.go:53-55). The API server
// does this on every create, update and read, so a field it defaulted
// (no field manager owns it) comes back by itself in a new object.
func ApplySchemaDefaults(x any, schema map[string]any) {
	if schema == nil {
		return
	}
	switch x := x.(type) {
	case map[string]any:
		props := SchemaProps(schema)
		for k, p := range props {
			ps, _ := p.(map[string]any)
			def, ok := ps["default"]
			if !ok {
				continue
			}
			if cur, found := x[k]; !found || (cur == nil && ps["nullable"] != true) {
				x[k] = kube.NormalizeJSON(def)
			}
		}
		additional, _ := schema["additionalProperties"].(map[string]any)
		for k, v := range x {
			if ps, ok := props[k].(map[string]any); ok {
				ApplySchemaDefaults(v, ps)
			} else if additional != nil {
				if def, ok := additional["default"]; ok && v == nil && additional["nullable"] != true {
					v = kube.NormalizeJSON(def)
					x[k] = v
				}
				ApplySchemaDefaults(v, additional)
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return
		}
		for i := range x {
			if def, ok := items["default"]; ok && x[i] == nil && items["nullable"] != true {
				x[i] = kube.NormalizeJSON(def)
			}
			ApplySchemaDefaults(x[i], items)
		}
	}
}

// WithSchemaDefaults returns a copy of obj with the schema's defaults
// filled in (ApplySchemaDefaults); an empty object for nil.
func WithSchemaDefaults(obj map[string]any, schema map[string]any) map[string]any {
	out, _ := kube.NormalizeJSON(obj).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	ApplySchemaDefaults(out, schema)
	return out
}

// DefaultedBack reports whether removing a field that holds value changes
// nothing: its schema has a default, and the API server's defaulting puts
// exactly value back (the default with the defaults below it). Removing
// such a field is no change, so a reset neither sends nor lists it.
func DefaultedBack(schema map[string]any, value any) bool {
	def, ok := schema["default"]
	if !ok {
		return false
	}
	back, now := kube.NormalizeJSON(def), kube.NormalizeJSON(value)
	ApplySchemaDefaults(back, schema)
	ApplySchemaDefaults(now, schema)
	return kube.JSONEqual(back, now)
}

// LookupPath returns the value at path in a decoded JSON object.
func LookupPath(obj map[string]any, path []string) (any, bool) {
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

// LookupState returns the managementState of the object at path.
func LookupState(components map[string]any, path []string) (string, bool) {
	v, ok := LookupPath(components, append(append([]string{}, path...), "managementState"))
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// --- JSON Patch (RFC 6902) -------------------------------------------------

func PatchOp(op string, path []string, value any) kube.JSONPatchOp {
	o := kube.JSONPatchOp{Op: op, Path: kube.PointerOf(path)}
	if op != "remove" {
		raw, _ := json.Marshal(value)
		o.Value = raw
	}
	return o
}

// DSCAssign sets the value at Path (keys from the DSC's root, such as
// ["spec", "components", "kserve", "managementState"]).
type DSCAssign struct {
	Path  []string
	Value any
}

// BuildJSONPatch returns a JSON Patch that makes exactly these changes to
// obj and fails as a whole when anything it changes differs from what obj
// holds now: each replaced or removed value is tested first, and a value
// added where nothing is gets a test of the nearest object that exists (a
// concurrent change anywhere in it fails the patch rather than being
// overwritten). Missing objects on the way are added with the value. All
// tests come first, so they see the object as read.
func BuildJSONPatch(obj map[string]any, sets []DSCAssign, removes [][]string) ([]kube.JSONPatchOp, error) {
	var tests, changes []kube.JSONPatchOp
	tested := map[string]bool{}
	test := func(path []string, value any) {
		p := kube.PointerOf(path)
		if !tested[p] {
			tested[p] = true
			tests = append(tests, PatchOp("test", path, value))
		}
	}
	for _, r := range removes {
		v, ok := LookupPath(obj, r)
		if !ok {
			return nil, fmt.Errorf("%s is not in the DataScienceCluster", strings.Join(r, "."))
		}
		test(r, v)
		changes = append(changes, PatchOp("remove", r, nil))
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
		if cur, ok := LookupPath(obj, s.Path); ok {
			if kube.JSONEqual(cur, s.Value) {
				continue
			}
			test(s.Path, cur)
			changes = append(changes, PatchOp("replace", s.Path, s.Value))
			continue
		}
		k := 0
		for k < len(s.Path)-1 {
			v, ok := LookupPath(obj, s.Path[:k+1])
			if !ok {
				break
			}
			if _, isMap := v.(map[string]any); !isMap {
				return nil, fmt.Errorf("%s is not an object", strings.Join(s.Path[:k+1], "."))
			}
			k++
		}
		root := s.Path[:k+1]
		parent, _ := LookupPath(obj, s.Path[:k])
		if len(s.Path[:k]) == 0 {
			parent = obj
		}
		test(s.Path[:k], parent)
		key := kube.PointerOf(root)
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
			changes = append(changes, PatchOp("add", in.root, in.leaf))
			continue
		}
		changes = append(changes, PatchOp("add", in.root, in.value))
	}
	return append(tests, changes...), nil
}
