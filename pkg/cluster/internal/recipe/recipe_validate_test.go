package recipe

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const header = "apiVersion: " + APIVersion + "\nkind: " + Kind + "\nmetadata:\n  name: dashboard-dev\n"

// source is a YAML recipe with header and spec (its lines indented by two).
func source(spec string) string { return header + "spec:\n" + spec }

const latest = "  rhoai: {build: latest}\n"

// fullYAML uses every field.
const fullYAML = `apiVersion: rhoai-nightly-updater/v1alpha1
kind: EnvironmentRecipe
metadata:
  name: dashboard-dev
  description: "PR review cluster"
spec:
  rhoai:
    build: {tag: rhoai-3.6}
    ifInstalled: update
  dataScienceCluster:
    create: true
  project:
    mode: create
    name: recipe-dev
    displayName: "Recipe dev"
  features:
    pipelines: {enabled: true}
    mlflow:    {enabled: true, samples: true}
    models:    {enabled: true, model: "", exposure: maas, releasedImage: true}
    playground: {enabled: true}
    maas:      {enabled: true, database: cnpg, publish: true}
  dashboardDev:
    deploy: pr
    pr: 4321
    flavor: odh
`

// fullJSON is fullYAML as JSON.
const fullJSON = `{
  "apiVersion": "rhoai-nightly-updater/v1alpha1",
  "kind": "EnvironmentRecipe",
  "metadata": {"name": "dashboard-dev", "description": "PR review cluster"},
  "spec": {
    "rhoai": {"build": {"tag": "rhoai-3.6"}, "ifInstalled": "update"},
    "dataScienceCluster": {"create": true},
    "project": {"mode": "create", "name": "recipe-dev", "displayName": "Recipe dev"},
    "features": {
      "pipelines": {"enabled": true},
      "mlflow": {"enabled": true, "samples": true},
      "models": {"enabled": true, "model": "", "exposure": "maas", "releasedImage": true},
      "playground": {"enabled": true},
      "maas": {"enabled": true, "database": "cnpg", "publish": true}
    },
    "dashboardDev": {"deploy": "pr", "pr": 4321, "flavor": "odh"}
  }
}`

const pinnedImage = "quay.io/rhoai/rhoai-fbc-fragment@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidate_AcceptsAndFillsDefaults(t *testing.T) {
	want := types.EnvironmentRecipe{
		APIVersion: APIVersion, Kind: Kind,
		Metadata: types.RecipeMetadata{Name: "dashboard-dev", Description: "PR review cluster"},
		Spec: types.RecipeSpec{
			RHOAI:              types.RecipeRHOAI{Build: types.RecipeBuild{Tag: "rhoai-3.6"}, IfInstalled: IfInstalledUpdate},
			DataScienceCluster: types.RecipeDataScienceCluster{Create: true},
			Project:            &types.RecipeProject{Mode: ProjectCreate, Name: "recipe-dev", DisplayName: "Recipe dev"},
			Features: types.RecipeFeatures{
				Pipelines:  types.RecipeFeature{Enabled: true},
				MLflow:     types.RecipeMLflow{Enabled: true, Samples: true},
				Models:     types.RecipeModels{Enabled: true, Exposure: "maas", ReleasedImage: true},
				Playground: types.RecipeFeature{Enabled: true},
				MaaS:       types.RecipeMaaS{Enabled: true, Database: MaaSDatabaseCNPG, Publish: true},
			},
			DashboardDev: types.RecipeDashboardDev{Deploy: DashboardDevPR, PR: 4321, Flavor: "odh"},
		},
	}
	minimal := types.EnvironmentRecipe{
		APIVersion: APIVersion, Kind: Kind, Metadata: types.RecipeMetadata{Name: "dashboard-dev"},
		Spec: types.RecipeSpec{
			RHOAI:        types.RecipeRHOAI{Build: types.RecipeBuild{Latest: true}, IfInstalled: IfInstalledKeep},
			DashboardDev: types.RecipeDashboardDev{Deploy: DashboardDevNone, Flavor: "rhoai"},
		},
	}
	withMaaS := minimal
	withMaaS.Spec.Features.MaaS = types.RecipeMaaS{Enabled: true, Database: MaaSDatabaseDevelopment}
	withImage := minimal
	withImage.Spec.RHOAI.Build = types.RecipeBuild{Image: pinnedImage}
	withTaggedImage := minimal
	withTaggedImage.Spec.RHOAI.Build = types.RecipeBuild{Image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64)}
	existing := minimal
	existing.Spec.Project = &types.RecipeProject{Mode: ProjectExisting, Name: "default"}

	cases := []struct {
		name, source string
		want         types.EnvironmentRecipe
	}{
		{"every field, YAML", fullYAML, want},
		{"every field, JSON", fullJSON, want},
		{"JSON after white space", "\n\t " + fullJSON + "\n", want},
		{"minimal: defaults filled in", source(latest), minimal},
		{"minimal JSON", `{"apiVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"dashboard-dev"},"spec":{"rhoai":{"build":"latest"}}}`, minimal},
		{"MaaS database defaults to development", source(latest + "  features: {maas: {enabled: true}}\n"), withMaaS},
		{"a pinned image", source("  rhoai: {build: {image: \"" + pinnedImage + "\"}}\n"), withImage},
		{"a pinned image with its tag", source("  rhoai: {build: {image: \"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64) + "\"}}\n"), withTaggedImage},
		{"an existing project may have any valid name", source(latest + "  project: {mode: existing, name: default}\n"), existing},
		{"a comment and a document start", "# my recipe\n---\n" + source(latest), minimal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, problems := Validate([]byte(tc.source))
			if len(problems) != 0 {
				t.Fatalf("problems %+v", problems)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// What Validate returns reads back the same as JSON (the wire format):
// "latest" and the object forms of the build survive the round trip.
func TestValidate_RoundTripsThroughJSON(t *testing.T) {
	for _, src := range []string{fullYAML, source(latest), source("  rhoai: {build: {image: \"" + pinnedImage + "\"}}\n")} {
		first, problems := Validate([]byte(src))
		if len(problems) != 0 {
			t.Fatalf("problems %+v", problems)
		}
		wire, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		again, problems := Validate(wire)
		if len(problems) != 0 || !reflect.DeepEqual(again, first) {
			t.Fatalf("%s read back as %+v, problems %+v", wire, again, problems)
		}
	}
}

// A source that cannot be read strictly is refused as a whole, with one
// problem that says why.
func TestValidate_RefusesWhatItCannotReadStrictly(t *testing.T) {
	big := source(latest) + "# " + strings.Repeat("x", MaxSourceBytes) + "\n"
	cases := []struct {
		name, source, want string
	}{
		{"over 16 KiB", big, "at most 16384 (16 KiB)"},
		{"empty", " \n\t", "the recipe is empty"},
		{"unknown YAML field", source(latest + "  extra: true\n"), "field extra not found"},
		{"unknown nested YAML field", source("  rhoai: {build: latest, channel: fast}\n"), "field channel not found"},
		{"unknown JSON member", `{"apiVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"x"},"spec":{"rhoai":{"build":"latest"},"extra":1}}`, `unknown object member name "extra"`},
		{"JSON member names match exactly", `{"APIVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"x"},"spec":{"rhoai":{"build":"latest"}}}`, `unknown object member name "APIVersion"`},
		{"JSON member given twice", `{"apiVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"x"},"spec":{"rhoai":{"build":"latest"},"features":{"mlflow":{"enabled":false,"enabled":true}}}}`, `duplicate object member name "enabled"`},
		{"YAML key given twice", source(latest + "  features: {mlflow: {enabled: false, enabled: true}}\n"), "already defined"},
		{"data after the JSON value", `{"apiVersion":"` + APIVersion + `"} {"kind":"` + Kind + `"}`, "not valid JSON"},
		{"two YAML documents", source(latest) + "---\n" + source(latest), "more than one YAML document"},
		{"a YAML anchor and alias", header + "spec:\n  rhoai: &r {build: latest}\n  dashboardDev: *r\n", "anchor or alias (line 6)"},
		{"a YAML merge key", header + "spec:\n  rhoai: {build: latest}\n  features:\n    mlflow: &on {enabled: true}\n    pipelines:\n      <<: *on\n", "anchor or alias"},
		{"not YAML", "spec: [unclosed\n", "not valid YAML"},
		{"a wrong YAML type", source(latest + "  dashboardDev: {deploy: pr, pr: \"12\"}\n"), "cannot unmarshal"},
		{"a wrong JSON type", `{"apiVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"x"},"spec":{"rhoai":{"build":"latest"},"dashboardDev":{"deploy":"pr","pr":"12"}}}`, "not valid JSON"},
		{"a build that is no form", source("  rhoai: {build: nightly}\n"), `build "nightly": use latest`},
		{"a build object with another key", source("  rhoai: {build: {tag: rhoai-3.6, digest: abc}}\n"), "field digest not found"},
		{"a JSON build object with another member", `{"apiVersion":"` + APIVersion + `","kind":"` + Kind + `","metadata":{"name":"x"},"spec":{"rhoai":{"build":{"Tag":"rhoai-3.6"}}}}`, `unknown object member name "Tag"`},
		// No secrets: the schema has no field for one.
		{"a pull secret", source(latest + "  pullSecret: {auths: {}}\n"), "field pullSecret not found"},
		{"a MaaS connection URL", source(latest + "  features: {maas: {enabled: true, database: external, connectionURL: \"postgresql://u:p@db/maas\"}}\n"), "field connectionURL not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, problems := Validate([]byte(tc.source))
			if len(problems) != 1 || problems[0].Field != "" || !strings.Contains(problems[0].Message, tc.want) {
				t.Fatalf("problems %+v, want one with %q", problems, tc.want)
			}
			if !reflect.DeepEqual(got, types.EnvironmentRecipe{}) {
				t.Fatalf("returned a recipe with the refusal: %+v", got)
			}
		})
	}
}

// Every rule is checked and every broken one reported, with its field.
func TestValidate_ReportsEveryBrokenRule(t *testing.T) {
	cases := []struct {
		name   string
		source string
		fields []string
		want   string // in one of the messages
	}{
		{"unknown apiVersion", strings.Replace(source(latest), "v1alpha1", "v1beta1", 1), []string{"apiVersion"}, `apiVersion "rhoai-nightly-updater/v1beta1" is not one this updater reads; it reads rhoai-nightly-updater/v1alpha1`},
		{"missing apiVersion and kind", "metadata: {name: x}\nspec:\n" + latest, []string{"apiVersion", "kind"}, `kind "" is not a recipe`},
		{"another kind", strings.Replace(source(latest), "kind: "+Kind, "kind: DataScienceCluster", 1), []string{"kind"}, "use EnvironmentRecipe"},
		{"no name", strings.Replace(source(latest), "  name: dashboard-dev\n", "  description: x\n", 1), []string{"metadata.name"}, "A name is required."},
		{"a name that is no DNS label", strings.Replace(source(latest), "dashboard-dev", "Dashboard_Dev", 1), []string{"metadata.name"}, `Name "Dashboard_Dev" is not a valid name`},
		{"a name of 64 characters", strings.Replace(source(latest), "dashboard-dev", strings.Repeat("a", 64), 1), []string{"metadata.name"}, "at most 63"},
		{"a description of 257 characters", strings.Replace(source(latest), "  name: dashboard-dev\n", "  name: dashboard-dev\n  description: "+strings.Repeat("é", 257)+"\n", 1), []string{"metadata.description"}, "257 characters; at most 256"},
		{"no build", source("  rhoai: {ifInstalled: keep}\n"), []string{"spec.rhoai.build"}, "A build is required"},
		{"no rhoai at all", source("  dataScienceCluster: {create: true}\n"), []string{"spec.rhoai.build"}, "A build is required"},
		{"a tag and an image", source("  rhoai: {build: {tag: rhoai-3.6, image: \"" + pinnedImage + "\"}}\n"), []string{"spec.rhoai.build"}, "Give one build"},
		{"a tag that is no nightly line", source("  rhoai: {build: {tag: \"3.6\"}}\n"), []string{"spec.rhoai.build.tag"}, `Tag "3.6" is not a nightly line`},
		{"an image not pinned by digest", source("  rhoai: {build: {image: \"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6\"}}\n"), []string{"spec.rhoai.build.image"}, "pinned by digest"},
		{"an image of another repository", source("  rhoai: {build: {image: \"quay.io/someone/rhoai-fbc-fragment@sha256:" + strings.Repeat("a", 64) + "\"}}\n"), []string{"spec.rhoai.build.image"}, "is not a build of quay.io/rhoai/rhoai-fbc-fragment"},
		{"an unknown ifInstalled", source("  rhoai: {build: latest, ifInstalled: replace}\n"), []string{"spec.rhoai.ifInstalled"}, "use keep or update"},
		{"a project without mode", source(latest + "  project: {name: recipe-dev}\n"), []string{"spec.project.mode"}, "A project mode is required"},
		{"an unknown project mode", source(latest + "  project: {mode: adopt, name: recipe-dev}\n"), []string{"spec.project.mode"}, `Project mode "adopt"`},
		{"a project name that is no DNS label", source(latest + "  project: {mode: existing, name: My.Project}\n"), []string{"spec.project.name"}, "is not a valid name"},
		{"a project without name", source(latest + "  project: {mode: create}\n"), []string{"spec.project.name"}, "A name is required."},
		{"an OpenShift project", source(latest + "  project: {mode: create, name: openshift-config}\n"), []string{"spec.project.name"}, "a name of the platform or of this updater"},
		{"project openshift", source(latest + "  project: {mode: create, name: openshift}\n"), []string{"spec.project.name"}, "a recipe does not create it"},
		{"a Kubernetes project", source(latest + "  project: {mode: create, name: kube-system}\n"), []string{"spec.project.name"}, "a recipe does not create it"},
		{"project default", source(latest + "  project: {mode: create, name: default}\n"), []string{"spec.project.name"}, "a recipe does not create it"},
		{"an RHOAI project", source(latest + "  project: {mode: create, name: redhat-ods-applications}\n"), []string{"spec.project.name"}, "a recipe does not create it"},
		{"the S3 storage's project", source(latest + "  project: {mode: create, name: seaweedfs}\n"), []string{"spec.project.name"}, "a recipe does not create it"},
		{"a display name of 257 characters", source(latest + "  project: {mode: create, name: recipe-dev, displayName: \"" + strings.Repeat("x", 257) + "\"}\n"), []string{"spec.project.displayName"}, "at most 256"},
		{"pipelines without a project", source(latest + "  features: {pipelines: {enabled: true}}\n"), []string{"spec.features.pipelines.enabled"}, "Pipelines needs a project"},
		{"models without a project", source(latest + "  features: {models: {enabled: true}}\n"), []string{"spec.features.models.enabled"}, "Models needs a project"},
		{"the playground without a model or project", source(latest + "  features: {playground: {enabled: true}}\n"), []string{"spec.features.playground.enabled", "spec.features.playground.enabled"}, "The playground needs a model"},
		{"MLflow samples without MLflow or a project", source(latest + "  features: {mlflow: {samples: true}}\n"), []string{"spec.features.mlflow.samples", "spec.features.mlflow.samples"}, "The MLflow samples need MLflow"},
		{"a model not in the catalog", source(latest + "  features: {models: {model: gpt-5}}\n"), []string{"spec.features.models.model"}, `Model "gpt-5" is not in this updater's model catalog`},
		{"exposure maas without MaaS", source(latest + "  project: {mode: create, name: p}\n  features: {models: {enabled: true, exposure: maas}}\n"), []string{"spec.features.models.exposure"}, "Exposure maas needs Models as a Service"},
		{"exposure gateway", source(latest + "  features: {models: {exposure: gateway}}\n"), []string{"spec.features.models.exposure"}, "needs a gateway name, which a recipe does not take"},
		{"an unknown exposure", source(latest + "  features: {models: {exposure: public}}\n"), []string{"spec.features.models.exposure"}, `Exposure "public"`},
		{"MaaS with an external database", source(latest + "  features: {maas: {enabled: true, database: external}}\n"), []string{"spec.features.maas.database"}, "its connection URL holds a password, and a recipe holds no secrets"},
		{"an external database while MaaS is off", source(latest + "  features: {maas: {database: external}}\n"), []string{"spec.features.maas.database"}, "a recipe holds no secrets"},
		{"an unknown MaaS database", source(latest + "  features: {maas: {enabled: true, database: sqlite}}\n"), []string{"spec.features.maas.database"}, `Database "sqlite"`},
		{"publishing without a model", source(latest + "  features: {maas: {enabled: true, publish: true}}\n"), []string{"spec.features.maas.publish"}, "Publishing through MaaS needs a model"},
		{"a PR with deploy main", source(latest + "  dashboardDev: {deploy: main, pr: 12}\n"), []string{"spec.dashboardDev.pr"}, "pr is only for deploy: pr"},
		{"a PR without deploy", source(latest + "  dashboardDev: {pr: 12}\n"), []string{"spec.dashboardDev.pr"}, "pr is only for deploy: pr"},
		{"deploy pr without a PR", source(latest + "  dashboardDev: {deploy: pr}\n"), []string{"spec.dashboardDev.pr"}, "needs the odh-dashboard pull request number"},
		{"deploy pr with a negative PR", source(latest + "  dashboardDev: {deploy: pr, pr: -3}\n"), []string{"spec.dashboardDev.pr"}, "needs the odh-dashboard pull request number"},
		{"an unknown deploy", source(latest + "  dashboardDev: {deploy: branch}\n"), []string{"spec.dashboardDev.deploy"}, `deploy "branch"`},
		{"an unknown flavor", source(latest + "  dashboardDev: {deploy: main, flavor: upstream}\n"), []string{"spec.dashboardDev.flavor"}, `flavor "upstream"`},
		{"every problem at once", "kind: Recipe\nmetadata: {name: X}\nspec:\n  rhoai: {ifInstalled: never}\n  features: {pipelines: {enabled: true}, maas: {database: external}}\n  dashboardDev: {deploy: pr}\n",
			[]string{"apiVersion", "kind", "metadata.name", "spec.rhoai.build", "spec.rhoai.ifInstalled", "spec.features.pipelines.enabled", "spec.features.maas.database", "spec.dashboardDev.pr"}, "A build is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := Validate([]byte(tc.source))
			var fields, messages []string
			for _, p := range problems {
				fields = append(fields, p.Field)
				messages = append(messages, p.Message)
			}
			if !slices.Equal(fields, tc.fields) {
				t.Fatalf("fields %q, want %q; problems %+v", fields, tc.fields, problems)
			}
			if !strings.Contains(strings.Join(messages, "\n"), tc.want) {
				t.Fatalf("no message with %q: %q", tc.want, messages)
			}
		})
	}
}
