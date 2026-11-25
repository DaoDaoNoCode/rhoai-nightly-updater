import { describe, expect, it } from "vitest";
import { outcomeTitle, outcomeVariant } from "./outcomes";

// pkg/cluster/internal/envkit SetupComponentBlock: the DataScienceCluster could
// not be read at any served version, so a setup was refused before any change
// (errorCode "prerequisites_unknown").
const RESULT = {
  success: false,
  errorCode: "prerequisites_unknown",
  message: "Cannot read whether Data Science Pipelines is turned on: the DataScienceCluster could not be read (HTTP 500). Nothing was changed.",
};

describe("prerequisites that could not be checked", () => {
  it("is a warning that says the check could not be made, not a failure", () => {
    expect(outcomeVariant(RESULT)).toBe("warning");
    expect(outcomeTitle(RESULT)).toBe("Blocked: prerequisites could not be checked");
  });
});
