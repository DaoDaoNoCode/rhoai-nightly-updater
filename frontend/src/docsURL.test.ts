import { describe, expect, it } from "vitest";
import { docsURL } from "./constants";

describe("docsURL", () => {
  it("links the running release's tag on the mirror, main for a dev build", () => {
    expect(docsURL("docs/GITOPS.md", "11-profiles", "v5.0.0")).toBe("https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/v5.0.0/docs/GITOPS.md#11-profiles");
    expect(docsURL("docs/UPGRADING.md", "patch-and-minor-releases", "v5.0.0-alpha.2")).toBe("https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/v5.0.0-alpha.2/docs/UPGRADING.md#patch-and-minor-releases");
    for (const dev of ["dev", "v5.0.0-dirty", "", undefined]) {
      expect(docsURL("docs/GITOPS.md", undefined, dev)).toBe("https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/main/docs/GITOPS.md");
    }
  });
});
