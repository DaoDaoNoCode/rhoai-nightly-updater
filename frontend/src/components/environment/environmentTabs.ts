/** The tabs, in the URL as ?tab= (overview is the default and is left out). */
export const ENVIRONMENT_TABS = ["overview", "pipelines", "mlflow", "models", "playground", "maas"] as const;
export type EnvironmentTab = (typeof ENVIRONMENT_TABS)[number];
export const TAB_TITLES: Record<EnvironmentTab, string> = { overview: "Overview", pipelines: "Pipelines", mlflow: "MLflow", models: "Models", playground: "Playground", maas: "MaaS" };
/** How many parts each feature tab lists: as many skeleton rows show while its status loads. */
export const TAB_PARTS: Record<Exclude<EnvironmentTab, "overview" | "maas">, number> = { pipelines: 2, mlflow: 1, models: 2, playground: 2 };
/** The tabs with a project picker. */
export const PROJECT_TABS: EnvironmentTab[] = ["pipelines", "mlflow", "models", "playground"];
/** Tabs of the earlier layout: links to them open Pipelines. */
export const OLD_TABS = ["cluster", "projects"];

export const BUSY_REASON = "Another operation is in progress";

/** The feature a run of the dialog belongs to. */
export type RunFeature = "pipelines" | "mlflow" | "models" | "playground" | "maas" | "samples" | "other";
