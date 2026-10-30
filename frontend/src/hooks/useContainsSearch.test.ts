import { describe, expect, it } from "vitest";
import { BUILT_BEFORE_MARGIN_MS, builtBefore, cutoffOf } from "./useContainsSearch";

describe("the date cutoff", () => {
  const merged = "2026-10-05T21:16:13Z";
  const cutoff = cutoffOf({ repo: "opendatahub-io/odh-dashboard", pr: 1, mergedAt: merged });

  it("is the merge or the commit date", () => {
    expect(cutoff).toBe(Date.parse(merged));
    expect(cutoffOf({ repo: "opendatahub-io/odh-dashboard", commit: "a".repeat(40), commitDate: "2026-10-05T10:00:00Z" })).toBe(Date.parse("2026-10-05T10:00:00Z"));
    expect(cutoffOf({ repo: "opendatahub-io/odh-dashboard", pr: 1 })).toBeNull();
    expect(cutoffOf(null)).toBeNull();
  });

  it("skips a build only when it was built more than an hour before the date (clock skew)", () => {
    expect(BUILT_BEFORE_MARGIN_MS).toBe(3_600_000);
    expect(builtBefore("2026-10-05T20:46:13Z", cutoff)).toBe(false); // 30 minutes before: checked
    expect(builtBefore("2026-10-05T20:16:13Z", cutoff)).toBe(false); // exactly an hour: checked
    expect(builtBefore("2026-10-05T19:16:13Z", cutoff)).toBe(true); // 2 hours before: skipped
    expect(builtBefore("2026-10-03T21:00:00Z", cutoff)).toBe(true);
    expect(builtBefore("2026-10-06T02:00:00Z", cutoff)).toBe(false);
  });

  it("never skips a build whose date is unknown", () => {
    expect(builtBefore(undefined, cutoff)).toBe(false);
    expect(builtBefore("not a date", cutoff)).toBe(false);
    expect(builtBefore("2026-10-03T21:00:00Z", null)).toBe(false);
  });
});
