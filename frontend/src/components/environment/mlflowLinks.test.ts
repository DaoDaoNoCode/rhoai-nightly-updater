import { describe, expect, it } from "vitest";
import { mlflowOpenHref } from "./UseIt";

describe("MLflow links", () => {
  it("open MLflow's experiments in the dashboard, with the project as its workspace", () => {
    expect(mlflowOpenHref("https://rh-ai.apps.example.com/", "https://rh-ai.apps.example.com/mlflow", null)).toBe("https://rh-ai.apps.example.com/develop-train/mlflow/experiments");
    expect(mlflowOpenHref("https://rh-ai.apps.example.com", "https://rh-ai.apps.example.com/mlflow", "my proj")).toBe("https://rh-ai.apps.example.com/develop-train/mlflow/experiments?workspace=my%20proj");
  });

  it("fall back to MLflow's own UI when the dashboard's address is not known or not https", () => {
    expect(mlflowOpenHref(undefined, "https://mlflow.apps.example.com", "dev")).toBe("https://mlflow.apps.example.com/#/experiments?workspace=dev");
    expect(mlflowOpenHref("http://rh-ai.example.com", "https://mlflow.apps.example.com", null)).toBe("https://mlflow.apps.example.com");
  });
});
