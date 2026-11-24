/** Tabs are selected via ?tab= in the query string; omit the parameter for overview. */
export const ENVIRONMENT_TABS = ["overview", "storage", "pipelines", "workbench", "mlflow", "model-registry", "models", "playground", "maas"] as const;
export type EnvironmentTab = (typeof ENVIRONMENT_TABS)[number];
export const TAB_TITLES: Record<EnvironmentTab, string> = { overview: "Overview", storage: "S3 storage", pipelines: "Pipelines", workbench: "Workbench", mlflow: "MLflow", "model-registry": "Model registry", models: "Models", playground: "Playground", maas: "MaaS" };
/** Each feature tab's part count drives how many skeleton rows appear until status finishes loading. */
export const TAB_PARTS: Record<Exclude<EnvironmentTab, "overview" | "maas">, number> = { storage: 1, pipelines: 2, workbench: 1, mlflow: 1, "model-registry": 2, models: 2, playground: 2 };
/** Only these tabs expose a project picker; S3 storage is cluster-wide and does not. */
export const PROJECT_TABS: EnvironmentTab[] = ["pipelines", "workbench", "mlflow", "models", "playground"];
/** Names from a prior navigation scheme; hits on these routes land on Pipelines instead. */
export const OLD_TABS = ["cluster", "projects"];
/** Href for the S3 storage tab—point any storage-related link here. */
export const STORAGE_HREF = "?tab=storage";

export const BUSY_REASON = "Another operation is in progress";

/** Which product area a particular dialog invocation is tied to. */
export type RunFeature = "storage" | "pipelines" | "workbench" | "mlflow" | "model-registry" | "models" | "playground" | "maas" | "samples" | "other";
