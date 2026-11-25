import { describe, expect, it } from "vitest";
import { DEFAULT_RECIPE_FORM, cleanupParts, componentName, componentsSplit, leftInPlace, playgroundThroughMaaS, sharedLeft, withComponentsBack } from "./recipeForm";
import { planText } from "./RecipeReviewModal";
import { contract } from "../../test/contract";
import type { RecipeRun, ResourcesStatus } from "../../types";

describe("recipe Clean up and plan words", () => {
  it("names DataScienceCluster components as RHOAI does, never by their keys (U1)", () => {
    expect(["aipipelines", "mlflowoperator", "modelregistry", "kserve", "kueue"].map(componentName)).toEqual(
      ["Data Science Pipelines", "MLflow", "Model registry", "KServe", "Kueue"]);
    // Every key the feature setups turn on (pkg/cluster/internal/features featureSetups) has a name.
    const keys = ["aipipelines", "datasciencepipelines", "mlflowoperator", "aiHub", "modelregistry", "kserve", "ogx", "llamastackoperator", "dashboard", "kueue"];
    expect(keys.filter((k) => componentName(k) === k)).toEqual([]);
    expect(["aiHub", "ogx"].map(componentName)).toEqual(["AI hub", "OGX"]);
    const parts = cleanupParts({ components: ["aipipelines", "kserve"] });
    expect(parts[0].title).toBe("The DataScienceCluster components the run turned on: Data Science Pipelines and KServe");
  });

  it("says whether removing distributed workloads turns the dashboard setting back, and what stays (U2, L3)", () => {
    const [on] = cleanupParts({ kueue: true }, true);
    const [off] = cleanupParts({ kueue: true }, false);
    expect(on.description).toContain("it turns the dashboard's Kueue setting back off, which the run turned on");
    expect(off.description).toContain("the dashboard's Kueue setting stays as it is");
    for (const p of [on, off]) {
      expect(p.description).toContain("the ClusterQueue and ResourceFlavor are cluster-wide objects; only this project uses them");
      expect(p.description).toContain("The Kueue operator and cert-manager stay");
      expect(p.description).not.toContain("Others may use it");
      expect(p.chosen).toBe(false);
    }
  });

  it("previews only the components the backend would turn back (L12)", () => {
    const run = contract.recipeRunFeatures().run!;
    const status = { mlflow: { deployed: false, ready: false } } as unknown as ResourcesStatus;
    const offered = { recipeProjects: ["recipe-full"], modelRegistry: true, kueue: true, components: ["kserve", "modelregistry", "kueue"] };
    // Unchecked registry and queue choices mean those components stay enabled.
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"] }, offered)).toEqual({
      back: ["KServe"], stay: ["Model registry (the model registry stays)", "Kueue (the distributed workloads setup stays)"], backKeys: ["kserve"],
    });
    // Once checked, they appear in the revert set as well.
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"], modelRegistry: true, kueue: true }, offered).back).toEqual(["KServe", "Model registry", "Kueue"]);
    // Omitted from the offer (outside this run) — not shown among items to turn back.
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"] }, { ...offered, modelRegistry: false, kueue: false }).stay).toEqual([
      "Model registry (the model registry is not removed here)", "Kueue (the distributed workloads setup is not removed here)",
    ]);
    // RHOAI 3.6 names the model registry component aiHub: it stays on while the registry stays, as modelregistry does.
    const on36 = { ...offered, components: ["kserve", "aiHub", "kueue"] };
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"] }, on36).stay).toEqual([
      "AI hub (the model registry stays)", "Kueue (the distributed workloads setup stays)",
    ]);
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"], modelRegistry: true, kueue: true }, on36).back).toEqual(["KServe", "AI hub", "Kueue"]);
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"] }, { ...on36, modelRegistry: false }).stay[0]).toBe("AI hub (the model registry is not removed here)");
  });

  it("keeps KServe on for a model the status shows in a project that stays, not for one that failed or is gone", () => {
    const run = contract.recipeRunFeatures().run!;
    const offered = { recipeProjects: ["recipe-full"], components: ["kserve"] };
    const model = (deployed: boolean, namespace = "recipe-full") => ({ models: { catalog: null, items: [{ name: "m", namespace, deployed, ready: deployed }] } }) as unknown as ResourcesStatus;
    const kept = { recipeProjects: [] };
    expect(componentsSplit(model(true), run, kept, offered).stay).toEqual(["KServe (the model in recipe-full stays)"]);
    // The recipe's model failed or was deleted: nothing keeps KServe on, so it goes back.
    expect(componentsSplit(model(false), run, kept, offered).backKeys).toEqual(["kserve"]);
    expect(componentsSplit({} as ResourcesStatus, run, kept, offered).backKeys).toEqual(["kserve"]);
    // A model in another project that stays keeps it on too (the server counts every model).
    expect(componentsSplit(model(true, "team-a"), run, { recipeProjects: ["recipe-full"] }, offered).stay).toEqual(["KServe (the model in team-a stays)"]);
    // Deleted with its project: it goes back.
    expect(componentsSplit(model(true), run, { recipeProjects: ["recipe-full"] }, offered).backKeys).toEqual(["kserve"]);
    // Only in a project that is no longer a data science project (models.outside): it still counts.
    const outside = { models: { catalog: null, items: [], outside: [{ name: "m", namespace: "old-proj", deployed: true }] } } as unknown as ResourcesStatus;
    expect(componentsSplit(outside, run, kept, offered).stay).toEqual(["KServe (the model in old-proj stays)"]);
    // Being deleted: it keeps nothing on.
    const going = { models: { catalog: null, items: [{ name: "m", namespace: "recipe-full", deployed: true, terminating: true }] } } as unknown as ResourcesStatus;
    expect(componentsSplit(going, run, kept, offered).backKeys).toEqual(["kserve"]);
    // Deployed but still starting (not ready): readiness does not decide, it keeps KServe on.
    const starting = { models: { catalog: null, items: [{ name: "m", namespace: "recipe-full", deployed: true, ready: false }] } } as unknown as ResourcesStatus;
    expect(componentsSplit(starting, run, kept, offered).stay).toEqual(["KServe (the model in recipe-full stays)"]);
    // No status: the recipe's own model counts while its project stays.
    expect(componentsSplit(null, run, kept, offered).stay).toEqual(["KServe (the model in recipe-full stays)"]);
  });

  it("lists the shared pieces that stay only for the features the run's steps set up", () => {
    const full = contract.recipeRunFeatures().run!;
    // Only Kueue ran: cert-manager and the Kueue operator, nothing of MaaS or Connectivity Link.
    const only = (ids: string[], features: Partial<RecipeRun["recipe"]["spec"]["features"]> = {}): RecipeRun => ({
      ...full, exposure: undefined,
      steps: full.steps.map((s) => ({ ...s, status: ids.includes(s.id) ? "success" : "pending" })),
      recipe: { ...full.recipe, spec: { ...full.recipe.spec, features: { ...full.recipe.spec.features, ...features } } },
    });
    expect(sharedLeft(only(["kueue"]))).toEqual(["The cert-manager Operator for Red Hat OpenShift", "The Red Hat build of Kueue"]);
    // A step that did not finish set nothing up.
    expect(sharedLeft(only([]))).toEqual([]);
    // A model reached through the inference gateway: Connectivity Link without Authorino TLS (MaaS sets that), and cert-manager.
    const gatewayModel = only(["model"], { models: { ...full.recipe.spec.features.models, exposure: "default-gateway" }, maas: { enabled: false } });
    expect(sharedLeft(gatewayModel)).toEqual(["Red Hat Connectivity Link, with its Kuadrant instance", "The cert-manager Operator for Red Hat OpenShift"]);
    // A model reached only inside the cluster needs no Connectivity Link.
    expect(sharedLeft(only(["model"], { models: { ...full.recipe.spec.features.models, exposure: "internal" }, maas: { enabled: false } }))).toEqual(["The cert-manager Operator for Red Hat OpenShift"]);
    // MaaS with a CloudNativePG database: its operator too.
    expect(sharedLeft(only(["maas"], { maas: { enabled: true, database: "cnpg" } }))).toEqual([
      "Red Hat Connectivity Link, with its Kuadrant instance and Authorino TLS", "The CloudNativePG operator", "GatewayClass openshift-default (OpenShift's shared class for gateways)",
    ]);
    // The tool's inference gateway the run's model used stays (Clean up everything deletes it); not for another way in.
    const status = { minio: null, mlflow: null, pipelineServers: [], inferenceGateway: { deployed: true, ready: true, managedByTool: true } } as unknown as ResourcesStatus;
    expect(leftInPlace(status, gatewayModel, { recipeProjects: ["recipe-full"] })).toEqual(["The inference gateway"]);
    expect(leftInPlace(status, only(["model"], { models: { ...full.recipe.spec.features.models, exposure: "internal" } }), { recipeProjects: ["recipe-full"] })).toEqual([]);
  });

  it("sends only the components the dialog says go back", () => {
    const req = { recipeProjects: ["recipe-full"], components: ["kserve", "kueue", "aiHub"] };
    expect(withComponentsBack(req, ["kserve"])).toEqual({ recipeProjects: ["recipe-full"], components: ["kserve"] });
    // None goes back: no components step at all.
    expect(withComponentsBack(req, [])).toEqual({ recipeProjects: ["recipe-full"] });
    // The reviewed enablement goes only with a component that is sent.
    expect(withComponentsBack({ ...req, componentIds: { kserve: "a", kueue: "b" } }, ["kserve", "aiHub"])).toEqual({ recipeProjects: ["recipe-full"], components: ["kserve", "aiHub"], componentIds: { kserve: "a" } });
    expect(withComponentsBack({ ...req, componentIds: { kueue: "b" } }, [])).toEqual({ recipeProjects: ["recipe-full"] });
    // A component reviewed without an ID goes with the backend's "none".
    expect(withComponentsBack({ ...req, componentIds: { kserve: "none" } }, ["kserve"]).componentIds).toEqual({ kserve: "none" });
    // The components part unticked stays unticked.
    expect(withComponentsBack({ recipeProjects: ["recipe-full"] }, ["kserve"])).toEqual({ recipeProjects: ["recipe-full"] });
  });

  it("a plan's messages do not repeat that nothing was changed (U3)", () => {
    expect(planText("Project p has a workbench this tool did not create; the recipe does not change it. Nothing was changed.")).toBe(
      "Project p has a workbench this tool did not create; the recipe does not change it.");
    expect(planText("Cannot resolve the build on Quay: 503. Nothing was changed; try again.")).toBe("Cannot resolve the build on Quay: 503. Try again.");
  });
});

describe("the playground through Models as a Service", () => {
  const form = { ...DEFAULT_RECIPE_FORM, models: true, playground: true, maasPublish: false, exposure: "" as const };
  it("is when the model is published through MaaS or reached through it, and the playground is on", () => {
    expect(playgroundThroughMaaS({ ...form, maas: true, maasPublish: true })).toBe(true);
    expect(playgroundThroughMaaS({ ...form, maas: true, exposure: "maas" })).toBe(true);
    expect(playgroundThroughMaaS({ ...form, maas: true })).toBe(false);
    expect(playgroundThroughMaaS({ ...form, maas: true, maasPublish: true, playground: false })).toBe(false);
    // Without MaaS, neither choice counts (the editor shows them off).
    expect(playgroundThroughMaaS({ ...form, maas: false, maasPublish: true, exposure: "maas" })).toBe(false);
  });
});
