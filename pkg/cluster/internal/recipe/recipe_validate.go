// Package recipe holds environment recipes: a declarative description of a
// dev cluster (an RHOAI build, the DataScienceCluster, a project and the
// Environment features) that the updater sets up in one run. This package
// reads and checks them (Validate). The schema is types.EnvironmentRecipe.
package recipe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	jsonv2 "encoding/json/v2"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"gopkg.in/yaml.v3"
)

const (
	// APIVersion is the only schema version this updater reads.
	APIVersion = "rhoai-nightly-updater/v1alpha1"
	// Kind is a recipe's kind.
	Kind = "EnvironmentRecipe"
	// MaxSourceBytes bounds a recipe file.
	MaxSourceBytes = 16 << 10

	maxDescriptionChars = 256
	maxDisplayNameChars = 256
)

// The values of the recipe's enumerations.
const (
	IfInstalledKeep   = "keep"
	IfInstalledUpdate = "update"

	ProjectCreate   = "create"
	ProjectExisting = "existing"

	MaaSDatabaseDevelopment = "development"
	MaaSDatabaseCNPG        = "cnpg"
	// maasDatabaseExternal is MaaS's own database kind for the user's
	// PostgreSQL. Recipes refuse it: its connection URL holds a password.
	maasDatabaseExternal = "external"

	DashboardDevNone = "none"
	DashboardDevMain = "main"
	DashboardDevPR   = "pr"
)

// dns1123Label is a Kubernetes name of a namespace: lower-case alphanumerics
// and '-', starting and ending with an alphanumeric, at most 63 characters.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// fbcImage is one build of the RHOAI catalog: the tool's FBC repository,
// pinned by digest (with or without a tag), as the Build Explorer's custom
// image. An FBC is installed with Automatic approval into an AllNamespaces
// OperatorGroup, so it never comes from another repository, and a recipe
// names the exact build.
var fbcImage = regexp.MustCompile(`^` + regexp.QuoteMeta(registry.FBCImage) + `(:[a-zA-Z0-9._-]+)?@sha256:[a-f0-9]{64}$`)

// Validate reads a recipe from source and checks it, failing closed: the
// recipe is usable only when the problems are empty, and then it comes
// back with its defaults filled in (rhoai.ifInstalled keep, maas.database
// development, dashboardDev.deploy none and flavor rhoai).
//
// source is JSON when it starts with '{' (after white space), else YAML.
// Both are read strictly: at most MaxSourceBytes, exactly one document or
// value, no field the schema does not have (names match exactly, and a
// field given twice is refused), and for YAML no anchors or aliases. Then
// every rule holds or is reported: the apiVersion and kind, every
// enumeration, the names, and the dependencies between the parts (see
// check). A recipe holds no secrets: the schema has no field for one, and
// MaaS's external database is refused.
//
// Validate reads nothing from the cluster; what depends on it (the build
// resolves, the one-time setup is done, the project name is free) is
// checked when a recipe is planned.
func Validate(source []byte) (types.EnvironmentRecipe, []types.RecipeProblem) {
	r, err := decode(source)
	if err != nil {
		return types.EnvironmentRecipe{}, []types.RecipeProblem{{Message: err.Error()}}
	}
	defaults(&r)
	return r, check(r)
}

// decode reads source strictly into a recipe (see Validate).
func decode(source []byte) (types.EnvironmentRecipe, error) {
	var r types.EnvironmentRecipe
	if len(source) > MaxSourceBytes {
		return r, fmt.Errorf("the recipe is %d bytes; at most %d (16 KiB) are read", len(source), MaxSourceBytes)
	}
	trimmed := bytes.TrimSpace(source)
	if len(trimmed) == 0 {
		return r, errors.New("the recipe is empty")
	}
	if trimmed[0] == '{' {
		// One JSON value (Unmarshal refuses data after it); member names
		// match exactly, a member given twice is refused, and so is one
		// the schema does not have.
		if err := jsonv2.Unmarshal(trimmed, &r, jsonv2.RejectUnknownMembers(true)); err != nil {
			return types.EnvironmentRecipe{}, fmt.Errorf("the recipe is not valid JSON for this schema: %w", err)
		}
		return r, nil
	}
	return decodeYAML(source)
}

// decodeYAML reads one YAML document without anchors or aliases into a
// recipe, refusing keys the schema does not have (yaml.v3 also refuses a
// key given twice in a mapping).
func decodeYAML(source []byte) (types.EnvironmentRecipe, error) {
	var r types.EnvironmentRecipe
	var doc yaml.Node
	nodes := yaml.NewDecoder(bytes.NewReader(source))
	if err := nodes.Decode(&doc); err != nil {
		return r, fmt.Errorf("the recipe is not valid YAML: %w", err)
	}
	var next yaml.Node
	if err := nodes.Decode(&next); !errors.Is(err, io.EOF) {
		return r, errors.New("the recipe has more than one YAML document; give one")
	}
	if where := anchorOrAlias(&doc); where != "" {
		return r, fmt.Errorf("the recipe uses a YAML anchor or alias (%s); write the values out", where)
	}
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	if err := strict.Decode(&r); err != nil {
		return types.EnvironmentRecipe{}, fmt.Errorf("the recipe is not valid YAML for this schema: %w", err)
	}
	return r, nil
}

// anchorOrAlias names the first anchor or alias in n ("line L"), or "".
func anchorOrAlias(n *yaml.Node) string {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return fmt.Sprintf("line %d", n.Line)
	}
	for _, child := range n.Content {
		if where := anchorOrAlias(child); where != "" {
			return where
		}
	}
	return ""
}

// defaults fills in the values a recipe may leave out.
func defaults(r *types.EnvironmentRecipe) {
	if r.Spec.RHOAI.IfInstalled == "" {
		r.Spec.RHOAI.IfInstalled = IfInstalledKeep
	}
	if r.Spec.Features.MaaS.Enabled && r.Spec.Features.MaaS.Database == "" {
		r.Spec.Features.MaaS.Database = MaaSDatabaseDevelopment
	}
	if r.Spec.DashboardDev.Deploy == "" {
		r.Spec.DashboardDev.Deploy = DashboardDevNone
	}
	if r.Spec.DashboardDev.Flavor == "" {
		r.Spec.DashboardDev.Flavor = dashdev.FlavorRHOAI
	}
}

// problems collects the problems of a recipe.
type problems []types.RecipeProblem

func (p *problems) add(field, format string, args ...any) {
	*p = append(*p, types.RecipeProblem{Field: field, Message: fmt.Sprintf(format, args...)})
}

// check reports every rule a decoded recipe (with its defaults) breaks.
func check(r types.EnvironmentRecipe) []types.RecipeProblem {
	var p problems
	if r.APIVersion != APIVersion {
		p.add("apiVersion", "apiVersion %q is not one this updater reads; it reads %s. A recipe of a newer schema needs a newer updater.", r.APIVersion, APIVersion)
	}
	if r.Kind != Kind {
		p.add("kind", "kind %q is not a recipe; use %s.", r.Kind, Kind)
	}
	checkName(&p, "metadata.name", r.Metadata.Name)
	if n := utf8.RuneCountInString(r.Metadata.Description); n > maxDescriptionChars {
		p.add("metadata.description", "The description has %d characters; at most %d.", n, maxDescriptionChars)
	}

	spec := r.Spec
	checkBuild(&p, spec.RHOAI.Build)
	switch spec.RHOAI.IfInstalled {
	case IfInstalledKeep, IfInstalledUpdate:
	default:
		p.add("spec.rhoai.ifInstalled", "ifInstalled %q: use %s or %s.", spec.RHOAI.IfInstalled, IfInstalledKeep, IfInstalledUpdate)
	}
	if spec.Project != nil {
		checkProject(&p, *spec.Project)
	}

	f := spec.Features
	needsProject := func(field, what string) {
		if spec.Project == nil {
			p.add(field, "%s needs a project: set spec.project.", what)
		}
	}
	if f.Pipelines.Enabled {
		needsProject("spec.features.pipelines.enabled", "Pipelines")
	}
	if f.MLflow.Samples {
		if !f.MLflow.Enabled {
			p.add("spec.features.mlflow.samples", "The MLflow samples need MLflow: set spec.features.mlflow.enabled.")
		}
		needsProject("spec.features.mlflow.samples", "The MLflow samples")
	}
	if f.Models.Enabled {
		needsProject("spec.features.models.enabled", "Models")
	}
	if !serving.ValidModelName(f.Models.Model) {
		p.add("spec.features.models.model", "Model %q is not in this updater's model catalog.", f.Models.Model)
	}
	switch f.Models.Exposure {
	case "", serving.ExposureDefault, serving.ExposureInternal:
	case serving.ExposureMaaS:
		if !f.MaaS.Enabled {
			p.add("spec.features.models.exposure", "Exposure %s needs Models as a Service: set spec.features.maas.enabled.", serving.ExposureMaaS)
		}
	case serving.ExposureGateway:
		p.add("spec.features.models.exposure", "Exposure %s needs a gateway name, which a recipe does not take; use %s, %s or %s, or leave it empty for the recommended one.", serving.ExposureGateway, serving.ExposureDefault, serving.ExposureMaaS, serving.ExposureInternal)
	default:
		p.add("spec.features.models.exposure", "Exposure %q: use %s, %s or %s, or leave it empty for the recommended one.", f.Models.Exposure, serving.ExposureDefault, serving.ExposureMaaS, serving.ExposureInternal)
	}
	if f.Playground.Enabled {
		if !f.Models.Enabled {
			p.add("spec.features.playground.enabled", "The playground needs a model: set spec.features.models.enabled.")
		}
		needsProject("spec.features.playground.enabled", "The playground")
	}
	switch f.MaaS.Database {
	case "", MaaSDatabaseDevelopment, MaaSDatabaseCNPG:
	case maasDatabaseExternal:
		p.add("spec.features.maas.database", "Database %s is not possible in a recipe: its connection URL holds a password, and a recipe holds no secrets. Use %s or %s, or set up MaaS with an external database on the Environment page.", maasDatabaseExternal, MaaSDatabaseDevelopment, MaaSDatabaseCNPG)
	default:
		p.add("spec.features.maas.database", "Database %q: use %s or %s.", f.MaaS.Database, MaaSDatabaseDevelopment, MaaSDatabaseCNPG)
	}
	if f.MaaS.Publish && f.MaaS.Enabled && !f.Models.Enabled {
		p.add("spec.features.maas.publish", "Publishing through MaaS needs a model: set spec.features.models.enabled.")
	}

	d := spec.DashboardDev
	switch d.Deploy {
	case DashboardDevNone, DashboardDevMain:
		if d.PR != 0 {
			p.add("spec.dashboardDev.pr", "pr is only for deploy: %s.", DashboardDevPR)
		}
	case DashboardDevPR:
		if d.PR <= 0 {
			p.add("spec.dashboardDev.pr", "deploy: %s needs the odh-dashboard pull request number in pr.", DashboardDevPR)
		}
	default:
		p.add("spec.dashboardDev.deploy", "deploy %q: use %s, %s or %s.", d.Deploy, DashboardDevNone, DashboardDevMain, DashboardDevPR)
	}
	if !dashdev.ValidFlavor(d.Flavor) {
		p.add("spec.dashboardDev.flavor", "flavor %q: use %s or %s.", d.Flavor, dashdev.FlavorRHOAI, dashdev.FlavorODH)
	}
	return p
}

// checkName checks a DNS-1123 label.
func checkName(p *problems, field, name string) {
	switch {
	case name == "":
		p.add(field, "A name is required.")
	case !dns1123Label.MatchString(name):
		p.add(field, "Name %q is not a valid name: at most 63 lower-case letters, digits and '-', starting and ending with a letter or digit.", name)
	}
}

// checkBuild checks that exactly one form of the build is given, and its
// shape: a tag of a nightly line (registry.CleanTagRegex; not a list of
// known releases), or a digest-pinned image of the RHOAI catalog
// repository. Whether it resolves is checked when the recipe is planned.
func checkBuild(p *problems, b types.RecipeBuild) {
	const field = "spec.rhoai.build"
	forms := 0
	for _, set := range []bool{b.Latest, b.Tag != "", b.Image != ""} {
		if set {
			forms++
		}
	}
	switch {
	case forms == 0:
		p.add(field, "A build is required: latest, {tag: rhoai-X.Y} or {image: %s@sha256:...}.", registry.FBCImage)
	case forms > 1:
		p.add(field, "Give one build: latest, a tag or an image.")
	case b.Tag != "" && !registry.CleanTagRegex.MatchString(b.Tag):
		p.add(field+".tag", "Tag %q is not a nightly line such as rhoai-3.6.", b.Tag)
	case b.Image != "" && !fbcImage.MatchString(b.Image):
		p.add(field+".image", "Image %q is not a build of %s pinned by digest (%s@sha256:<64 hex digits>).", b.Image, registry.FBCImage, registry.FBCImage)
	}
}

// checkProject checks the project's mode and names. A project the recipe
// creates must not take a name of the platform's, Kubernetes' or the
// tool's own (the S3 storage's); the RHOAI namespaces (redhat-*) are
// covered here, the ones read from the cluster (the applications
// namespace, the updater's own) when the recipe is planned.
func checkProject(p *problems, project types.RecipeProject) {
	switch project.Mode {
	case ProjectCreate, ProjectExisting:
	case "":
		p.add("spec.project.mode", "A project mode is required: %s (the recipe creates the project) or %s (a data science project you have).", ProjectCreate, ProjectExisting)
	default:
		p.add("spec.project.mode", "Project mode %q: use %s or %s.", project.Mode, ProjectCreate, ProjectExisting)
	}
	checkName(p, "spec.project.name", project.Name)
	if project.Mode == ProjectCreate && reservedProjectName(project.Name) {
		p.add("spec.project.name", "Project %q is a name of the platform or of this updater; a recipe does not create it. Choose another name.", project.Name)
	}
	if n := utf8.RuneCountInString(project.DisplayName); n > maxDisplayNameChars {
		p.add("spec.project.displayName", "The display name has %d characters; at most %d.", n, maxDisplayNameChars)
	}
}

// reservedProjectName reports whether name belongs to Kubernetes, OpenShift,
// Red Hat products or the tool's S3 storage.
func reservedProjectName(name string) bool {
	switch {
	case name == "default", name == storage.S3Namespace:
		return true
	case strings.HasPrefix(name, "openshift"), strings.HasPrefix(name, "kube-"), strings.HasPrefix(name, "redhat-"):
		return true
	}
	return false
}
