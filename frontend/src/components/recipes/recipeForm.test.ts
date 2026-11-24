import { describe, expect, it } from "vitest";
import { DEFAULT_RECIPE_FORM, cleanupParts, componentName, componentsSplit, playgroundThroughMaaS } from "./recipeForm";
import { planText } from "./RecipeReviewModal";
import { contract } from "../../test/contract";
import type { ResourcesStatus } from "../../types";

describe("recipe Clean up and plan words", () => {
  it("names DataScienceCluster components as RHOAI does, never by their keys (U1)", () => {
    expect(["aipipelines", "mlflowoperator", "modelregistry", "kserve", "kueue"].map(componentName)).toEqual(
      ["Data Science Pipelines", "MLflow", "Model registry", "KServe", "Kueue"]);
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
      back: ["KServe"], stay: ["Model registry (the model registry stays)", "Kueue (the distributed workloads setup stays)"],
    });
    // Once checked, they appear in the revert set as well.
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"], modelRegistry: true, kueue: true }, offered).back).toEqual(["KServe", "Model registry", "Kueue"]);
    // Omitted from the offer (outside this run) — not shown among items to turn back.
    expect(componentsSplit(status, run, { recipeProjects: ["recipe-full"] }, { ...offered, modelRegistry: false, kueue: false }).stay).toEqual([
      "Model registry (the model registry is not removed here)", "Kueue (the distributed workloads setup is not removed here)",
    ]);
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
