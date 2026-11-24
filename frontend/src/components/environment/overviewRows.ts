import type { PrerequisiteFeature, PrerequisitesStatus } from "../../types";
import type { FeatureState } from "./featureState";
import type { OverviewRow } from "./Overview";
import { withPrerequisites } from "./Prerequisites";
import { MLFLOW_SAMPLES_ID } from "./MLflowSamples";
import { PIPELINE_SAMPLE_ID } from "./PipelineSample";
import { STORAGE_HREF, TAB_TITLES } from "./environmentTabs";

/** The Overview's rows: each feature's summary state (with its prerequisites) and its tab's address. */
export function overviewRows(summaries: { storage: FeatureState; pipelines: FeatureState; workbench: FeatureState; mlflow: FeatureState; modelRegistry: FeatureState; models: FeatureState; playground: FeatureState; kueue: FeatureState; maas: FeatureState }, prereq: PrerequisitesStatus | null): OverviewRow[] {
  const { storage: storageNow, pipelines: pipelinesSummary, workbench: workbenchSummary, mlflow: mlflowNow, modelRegistry: modelRegistryNow, models: modelsSummary, playground: playgroundSummary, kueue: kueueNow, maas: maasNow } = summaries;
  // Once a feature works, its samples are the natural next step.
  const pipelinesSamples = pipelinesSummary.status === "ready" && !!pipelinesSummary.project;
  const prereqFor = (feature: PrerequisiteFeature) => prereq?.features?.[feature];
  return [
    {
      id: "storage", name: TAB_TITLES.storage, gives: "Object storage (S3) for the whole cluster: pipelines and other features", state: storageNow, href: STORAGE_HREF,
    },
    {
      id: "pipelines", name: "Pipelines", gives: "Run data science pipelines; artifacts in S3", state: withPrerequisites(pipelinesSummary, prereqFor("pipelines"), "pipelines"),
      href: `?tab=pipelines${pipelinesSummary.project ? `&project=${encodeURIComponent(pipelinesSummary.project)}` : ""}`,
      ...(pipelinesSamples ? { samples: { label: "Run a sample pipeline", focus: PIPELINE_SAMPLE_ID } } : {}),
    },
    {
      id: "workbench", name: TAB_TITLES.workbench, gives: "Develop in a Jupyter workbench, made as the dashboard makes it", state: workbenchSummary,
      href: `?tab=workbench${workbenchSummary.project ? `&project=${encodeURIComponent(workbenchSummary.project)}` : ""}`,
    },
    {
      id: "mlflow", name: "MLflow", gives: "Track experiments, runs and models", state: withPrerequisites(mlflowNow, prereqFor("mlflow"), "mlflow"), href: "?tab=mlflow",
      ...(mlflowNow.status === "ready" ? { samples: { label: "Add sample data", focus: MLFLOW_SAMPLES_ID } } : {}),
    },
    {
      id: "model-registry", name: TAB_TITLES["model-registry"], gives: "Register models and their versions; deploy them from the dashboard",
      state: withPrerequisites(modelRegistryNow, prereqFor("model-registry"), "model-registry"), href: "?tab=model-registry",
    },
    {
      id: "models", name: "Models", gives: "Serve a small chat model on CPU; call it or use the playground", state: withPrerequisites(modelsSummary, prereqFor("models"), "models"),
      href: `?tab=models${modelsSummary.project ? `&project=${encodeURIComponent(modelsSummary.project)}` : ""}`,
    },
    {
      id: "playground", name: "Playground", gives: "Chat with a project's models in the dashboard's Gen AI playground", state: withPrerequisites(playgroundSummary, prereqFor("playground"), "playground"),
      href: `?tab=playground${playgroundSummary.project ? `&project=${encodeURIComponent(playgroundSummary.project)}` : ""}`,
    },
    {
      id: "kueue", name: TAB_TITLES.kueue, gives: "Queue Ray workloads with Kueue; see them on the dashboard's Distributed workloads pages",
      state: withPrerequisites(kueueNow, prereqFor("kueue"), "kueue"),
      href: `?tab=kueue${kueueNow.project ? `&project=${encodeURIComponent(kueueNow.project)}` : ""}`,
    },
    {
      id: "maas", name: "MaaS", gives: "Offer models to everyone on the cluster with API keys and token limits", state: maasNow, href: "?tab=maas",
    },
  ];
}
