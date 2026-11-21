import { describe, expect, it } from "vitest";
import { describeError } from "./errors";
import { errorResult, outcomeTitle, outcomeVariant } from "./outcomes";
import { ApiError } from "./services/api";

// pkg/api operationErrorStatus: the API server refused a change because the
// object changed since it was read (HTTP 409, errorCode "conflict").
const MESSAGE = "k8s API error 409 (Conflict): Operation cannot be fulfilled on datascienceclusters.datasciencecluster.opendatahub.io \"default-dsc\": the object has been modified";

describe("conflict (the object changed since it was read)", () => {
  it("says the object changed, not that another operation runs", () => {
    const d = describeError(new ApiError({ status: 409, errorCode: "conflict", message: MESSAGE }), "Could not repair the DataScienceCluster");
    expect(d).toMatchObject({ title: "The object changed in the meantime", variant: "warning" });
    expect(d.body).toContain("the object has been modified");
    expect(d.hint).toMatch(/Reload/);
    expect(`${d.title} ${d.body} ${d.hint}`).not.toMatch(/Cluster busy|Another operation/);
  });

  it("keeps its errorCode as an operation result", () => {
    const r = errorResult(new ApiError({ status: 409, errorCode: "conflict", message: MESSAGE }), "Could not repair the DataScienceCluster");
    expect(r).toMatchObject({ success: false, errorCode: "conflict", message: MESSAGE });
    expect(outcomeTitle(r)).toBe("The object changed in the meantime");
    expect(outcomeVariant(r)).toBe("warning");
  });

  it("leaves 409 cluster_busy as it was", () => {
    const d = describeError(new ApiError({ status: 409, errorCode: "cluster_busy", message: "alice is running \"Update to nightly\". Wait for it to finish.", details: { operation: { id: "x" } } }));
    expect(d).toMatchObject({ title: "Cluster busy", variant: "warning" });
    expect(d.body).toContain("alice is running");
  });
});
