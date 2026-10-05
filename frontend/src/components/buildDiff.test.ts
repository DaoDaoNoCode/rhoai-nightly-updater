import { describe, expect, it } from "vitest";
import type { RelatedImage } from "../types";
import { classifySearch, compareBuilds, compareURL, imageDigest, imageTag, imagesBuiltFrom, installedBuild, shortBuildRef } from "./buildDiff";

const D1 = "sha256:4eff06d60bd10119e9c9bbdd9e69c3db14147ec4c655f7f8adb0ec9a4bf83e6d";
const IMG = `quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@${D1}`;

function ri(name: string, image: string, gitCommit?: string, gitURL = "https://github.com/red-hat-data-services/odh-dashboard"): RelatedImage {
  return { name, image, gitCommit, gitURL: gitCommit ? gitURL : undefined };
}

describe("image reference helpers", () => {
  it("digest, tag and short ref", () => {
    expect(imageDigest(IMG)).toBe(D1);
    expect(imageTag(IMG)).toBe("rhoai-3.6");
    expect(shortBuildRef(IMG)).toBe("rhoai-3.6 @ 4eff06d");
    expect(imageDigest("quay.io/x/y:tag")).toBe("");
    expect(imageTag("registry:5000/x/y@sha256:abc")).toBe("");
  });
});

describe("installedBuild", () => {
  const subscription = { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "stable-3.x", state: "AtLatestKnown" };
  const catalogSource = { exists: true, name: "rhoai-catalog-dev", image: IMG, state: "READY" };

  it("prefers status.nightly", () => {
    const b = installedBuild({ subscription, catalogSource, nightly: { installed: { image: IMG, tag: "rhoai-3.6", digest: D1, buildDate: "2026-10-01T12:02:38Z" } } });
    expect(b).toEqual({ image: IMG, digest: D1, tag: "rhoai-3.6", buildDate: "2026-10-01T12:02:38Z", source: "nightly" });
  });

  it("falls back to the nightly CatalogSource only when the Subscription uses it", () => {
    expect(installedBuild({ subscription, catalogSource })?.source).toBe("catalog");
    expect(installedBuild({ subscription: { ...subscription, source: "redhat-operators" }, catalogSource })).toBeNull();
    expect(installedBuild({ subscription, catalogSource: { ...catalogSource, image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6" } })).toBeNull();
    expect(installedBuild(null)).toBeNull();
  });
});

describe("compareBuilds (A08-5)", () => {
  const from = [
    ri("odh_dashboard_image", "r/dash@sha256:1", "aaaaaaa1"),
    ri("odh_dashboard_operator_image", "r/dop@sha256:1", "aaaaaaa1"),
    ri("odh_kserve_image", "r/kserve@sha256:1", "bbbbbbb1", "https://github.com/red-hat-data-services/kserve"),
    ri("odh_same_image", "r/same@sha256:1", "ccccccc1"),
    ri("odh_rebuilt_image", "r/reb@sha256:1", "ddddddd1", "https://github.com/red-hat-data-services/rebuilt"),
    ri("odh_nolabel_image", "r/nl@sha256:1"),
    ri("odh_gone_image", "r/gone@sha256:1"),
  ];
  const to = [
    ri("odh_dashboard_image", "r/dash@sha256:2", "aaaaaaa2"),
    ri("odh_dashboard_operator_image", "r/dop@sha256:2", "aaaaaaa2"),
    ri("odh_kserve_image", "r/kserve@sha256:2", "bbbbbbb2", "https://github.com/red-hat-data-services/kserve"),
    ri("odh_same_image", "r/same@sha256:1", "ccccccc1"),
    ri("odh_rebuilt_image", "r/reb@sha256:2", "ddddddd1", "https://github.com/red-hat-data-services/rebuilt"),
    ri("odh_nolabel_image", "r/nl@sha256:2"),
    ri("odh_new_image", "r/new@sha256:1"),
  ];

  it("groups changed images per repository and commit pair, with one compare link each", () => {
    const c = compareBuilds(from, to);
    expect(c.changedImages).toBe(5);
    expect(c.unchangedImages).toBe(1);
    expect(c.added.map((i) => i.name)).toEqual(["odh_new_image"]);
    expect(c.removed.map((i) => i.name)).toEqual(["odh_gone_image"]);
    expect(c.repos.map((r) => [r.repo, r.kind, r.images.length])).toEqual([
      ["red-hat-data-services/kserve", "changed", 1],
      ["red-hat-data-services/odh-dashboard", "changed", 2],
      ["red-hat-data-services/rebuilt", "rebuilt", 1],
      ["No git labels", "unknown", 1],
    ]);
    expect(compareURL(c.repos[1])).toBe("https://github.com/red-hat-data-services/odh-dashboard/compare/aaaaaaa1...aaaaaaa2");
    expect(compareURL(c.repos[3])).toBe("");
  });

  it("identical builds have no differences", () => {
    const c = compareBuilds(from, from);
    expect(c.repos).toEqual([]);
    expect(c.unchangedImages).toBe(from.length);
  });
});

describe("classifySearch and imagesBuiltFrom", () => {
  it.each([
    ["", "empty"],
    [IMG, "image"],
    ["quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", "image"],
    ["32a2131", "commit"],
    ["32A213123FBC434A602BA5B05E4A2022DD0D85D6", "commit"],
    ["#10085", "pr"],
    ["10085", "pr"],
    ["1234567", "commit"],
    ["not a thing", "invalid"],
    ["abc12", "invalid"],
  ])("%s -> %s", (input, kind) => {
    expect(classifySearch(input).kind).toBe(kind);
  });

  it("matches a full SHA or a prefix, case-insensitively", () => {
    const imgs = [ri("a", "x", "32a213123fbc434a602ba5b05e4a2022dd0d85d6"), ri("b", "y", "a3b6b581")];
    expect(imagesBuiltFrom(imgs, "32A2131").map((i) => i.name)).toEqual(["a"]);
    expect(imagesBuiltFrom(imgs, "a3b6b581").map((i) => i.name)).toEqual(["b"]);
    expect(imagesBuiltFrom(imgs, "ffffff0")).toEqual([]);
  });
});
