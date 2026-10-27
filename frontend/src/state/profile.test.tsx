import React from "react";
import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { useGitOpsProfile, useProfile } from "./AppInfo";
import { renderWithApp } from "../test/providers";
import { GITOPS_PROFILE_PAGES, gitOpsPolicyProblem, gitOpsProfileReason, isGitOpsProfile } from "../profile";
import type { VersionInfo } from "../types";

const Probe: React.FC = () => {
  const profile = useProfile();
  const gitOps = useGitOpsProfile();
  return <pre data-testid="probe">{JSON.stringify({ profile, gitOps })}</pre>;
};
const probe = () => JSON.parse(screen.getByTestId("probe").textContent || "{}");
const version = (profile?: "full" | "gitops"): VersionInfo => ({ version: "dev", commit: "x", buildDate: "x", templateOutdated: false, ...(profile ? { profile } : {}) });

describe("profile (docs/GITOPS.md, Profiles)", () => {
  it("is unknown until /api/version answers, then the backend's", async () => {
    let answer: (v: VersionInfo) => void = () => {};
    renderWithApp(<Probe />, { version: () => new Promise((r) => { answer = r; }) });
    expect(probe()).toEqual({ profile: null, gitOps: false });
    answer(version("gitops"));
    await waitFor(() => expect(probe()).toEqual({ profile: "gitops", gitOps: true }));
  });

  it("is full for an older backend without the field, or without /api/version", async () => {
    renderWithApp(<Probe />, { version: async () => version() });
    await waitFor(() => expect(probe().profile).toBe("full"));
    renderWithApp(<Probe />, { version: async () => { throw new TypeError("Failed to fetch"); } });
    await waitFor(() => expect(screen.getAllByTestId("probe").map((e) => JSON.parse(e.textContent || "{}").profile)).toEqual(["full", "full"]));
  });

  it("never asks for the Dashboard Dev state in the gitops profile (403 profile_gitops)", async () => {
    const dashboard = vi.fn(async () => { throw new Error("must not be called"); });
    renderWithApp(<Probe />, { version: async () => version("gitops"), dashboard });
    await waitFor(() => expect(probe().profile).toBe("gitops"));
    await new Promise((r) => setTimeout(r, 20));
    expect(dashboard).not.toHaveBeenCalled();
  });

  it("asks for it once the profile is known to be full", async () => {
    const dashboard = vi.fn(async () => ({ override: { active: false } }) as never);
    renderWithApp(<Probe />, { version: async () => version("full"), dashboard });
    await waitFor(() => expect(dashboard).toHaveBeenCalled());
  });

  it("keeps the pages the backend serves and says why a feature is missing", () => {
    expect([...GITOPS_PROFILE_PAGES].sort()).toEqual(["/", "/components", "/diagnostics"]);
    expect(isGitOpsProfile(version("gitops"))).toBe(true);
    expect(isGitOpsProfile(version("full"))).toBe(false);
    expect(isGitOpsProfile(null)).toBe(false);
    expect(gitOpsProfileReason("Reinstall")).toMatch(/^Reinstall is off: this updater runs the gitops profile/);
  });

  it("turns Update off while the gitops profile's admission policy is missing", () => {
    expect(gitOpsPolicyProblem({ ...version("gitops"), admissionPolicy: "enforced" })).toBeNull();
    expect(gitOpsPolicyProblem(version("gitops"))).toBeNull();
    expect(gitOpsPolicyProblem({ ...version("gitops"), admissionPolicy: "The policy is not in place" })).toBe("The policy is not in place");
    expect(gitOpsPolicyProblem({ ...version("full"), admissionPolicy: "x" })).toBeNull();
  });
});
