import { describe, expect, it } from "vitest";
import { dashboardOperatorLabel } from "./dashboardOperator";

describe("dashboardOperatorLabel", () => {
  it("names the Deployment the backend discovered", () => {
    expect(dashboardOperatorLabel("dashboard-module-manager")).toBe("dashboard-module-manager");
  });
  it("stays generic without one, never assuming dashboard-operator", () => {
    expect(dashboardOperatorLabel(undefined)).toBe("the dashboard operator");
    expect(dashboardOperatorLabel("")).toBe("the dashboard operator");
  });
});
