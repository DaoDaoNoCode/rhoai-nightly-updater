import { describe, expect, it } from "vitest";
import { repairPreview } from "./dscRepair";
import type { DSCCompatibility } from "../types";

const compat = (over: Partial<DSCCompatibility>): DSCCompatibility => ({
  invalidFields: [], missingComponents: [], extraComponents: [], ...over,
});

describe("repairPreview", () => {
  it("lists DataScienceCluster v3 parts of a reset with their blocks", () => {
    const p = repairPreview(compat({
      resetRemovals: ["dashboard.standard", "kserve", "kserve.nim"],
      removalBlocks: [
        { component: "dashboard.standard", reasons: ["Removing the dashboard deletes the Dashboard CR"] },
        { component: "kserve.nim", reasons: ["its operator has no ready pod"] },
        { component: "ray", reasons: ["only for remove-extra"] },
      ],
    }), "reset-defaults");
    expect(p.removals).toEqual(["dashboard.standard", "kserve", "kserve.nim"]);
    expect(p.blocked.map((b) => b.component)).toEqual(["dashboard.standard", "kserve.nim"]);
  });

  it("an extra component is blocked by a block on one of its parts", () => {
    const p = repairPreview(compat({
      extraComponents: ["kserve"],
      removalBlocks: [{ component: "kserve.nim", reasons: ["x"] }, { component: "kserveextra", reasons: ["y"] }],
    }), "remove-extra-components");
    expect(p.blocked.map((b) => b.component)).toEqual(["kserve.nim"]);
  });

  it("v2 names match exactly, as before", () => {
    const p = repairPreview(compat({
      resetRemovals: ["ray"],
      removalBlocks: [{ component: "ray", reasons: ["x"] }, { component: "trainer", reasons: ["y"] }],
    }), "reset-defaults");
    expect(p.blocked.map((b) => b.component)).toEqual(["ray"]);
    expect(repairPreview(compat({ resetRemovals: ["ray"] }), "remove-invalid")).toEqual({ removals: [], blocked: [] });
  });
});
