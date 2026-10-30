import React from "react";
import { describe, expect, it } from "vitest";
import { render as renderBare, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DashboardImages, runningLabel } from "./DashboardImages";
import type { DashboardContainerBuild, DashboardDevImage } from "../types";

const SHA = "21c06a3be1de89a8e18cf95cc7eef5a78add9481";
const DIGEST = "sha256:" + "a".repeat(64);

function image(container: string, over: Partial<DashboardDevImage> = {}): DashboardDevImage {
  return {
    deployment: container, container, envVar: "X", repository: "opendatahub/odh-dashboard",
    currentImage: `quay.io/opendatahub/odh-dashboard:${SHA}@${DIGEST}`, defaultImage: `registry.redhat.io/rhoai/odh-dashboard-rhel9@${DIGEST}`,
    ready: true, matchesTarget: true, ...over,
  };
}

function build(over: Partial<DashboardContainerBuild> = {}): DashboardContainerBuild {
  return {
    deployment: "rhods-dashboard", container: "rhods-dashboard", image: `quay.io/opendatahub/odh-dashboard:${SHA}@${DIGEST}`,
    running: "main", flavor: "rhoai", commit: SHA, repo: "opendatahub-io/odh-dashboard",
    commitURL: `https://github.com/opendatahub-io/odh-dashboard/commit/${SHA}`, buildDate: new Date(Date.now() - 2 * 3600_000).toISOString(),
    ...over,
  };
}

/** The card links to the Build Explorer, so it needs a router. */
function render(ui: React.ReactElement) {
  const wrap = (el: React.ReactElement) => <MemoryRouter>{el}</MemoryRouter>;
  const result = renderBare(wrap(ui));
  return { ...result, rerender: (next: React.ReactElement) => result.rerender(wrap(next)) };
}

const details = (builds: DashboardContainerBuild[], over: Partial<{ loading: boolean; rateLimited: boolean; retryAfterSeconds?: number; error: null }> = {}) =>
  ({ builds, loading: false, error: null, rateLimited: false, ...over });

describe("runningLabel", () => {
  it("reads the commit tags Deploy pins main builds under, on older backends without `running`", () => {
    expect(runningLabel(image("x", { currentImage: `quay.io/opendatahub/odh-dashboard:${SHA}@${DIGEST}` })).text).toBe("Latest main");
    expect(runningLabel(image("x", { currentImage: `quay.io/opendatahub/odh-dashboard:main-21c06a3@${DIGEST}` })).text).toBe("Latest main");
    expect(runningLabel(image("x", { currentImage: `quay.io/opendatahub/odh-dashboard:odh-stable@${DIGEST}` })).text).toBe("Latest main");
    expect(runningLabel(image("x", { currentImage: `quay.io/opendatahub/odh-dashboard:odh-pr-12@${DIGEST}` })).text).toBe("PR #12");
    expect(runningLabel(image("x", { currentImage: `quay.io/opendatahub/odh-dashboard:${SHA.slice(0, 39)}@${DIGEST}` })).text).toBe("Other image");
  });
});

describe("DashboardImages build details", () => {
  it("without build details a commit tag is shown short, never as 40 hex", () => {
    render(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" })]} defaultExpanded />);
    expect(screen.getByText("21c06a3")).toBeInTheDocument();
    expect(screen.queryByText(SHA)).not.toBeInTheDocument();
    expect(screen.getByText("RHOAI build")).toBeInTheDocument();
  });

  it("shows the short commit as a link to GitHub, when it was built, and the distance from main", () => {
    render(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" })]} defaultExpanded
      buildDetails={details([build({ divergence: { base: "main", baseHead: "b".repeat(40), status: "behind", behind: 3, ahead: 0, compareURL: "https://github.com/opendatahub-io/odh-dashboard/compare/x...main" }, commitDate: new Date(Date.now() - 5 * 3600_000).toISOString() })])} />);
    const link = screen.getByRole("link", { name: "21c06a3" });
    expect(link).toHaveAttribute("href", `https://github.com/opendatahub-io/odh-dashboard/commit/${SHA}`);
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.getByText("built 2h ago")).toBeInTheDocument();
    expect(screen.getByText("committed 5h ago")).toBeInTheDocument();
    // The Build Explorer search for this commit, on the same line; the commit URL carries the repository.
    const find = screen.getByRole("link", { name: "Find commit 21c06a3 in nightlies" });
    expect(find).toHaveAttribute("href", `/builds?q=${encodeURIComponent(`https://github.com/opendatahub-io/odh-dashboard/commit/${SHA}`)}`);
    // (The absolute dates in between are the screen-reader text of each time.)
    expect(find.parentElement?.textContent).toMatch(/^built 2h ago .* · committed 5h ago .* · Find in nightlies$/);
    expect(screen.getByText("3 commits behind main")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Compare/ })).toHaveAttribute("href", "https://github.com/opendatahub-io/odh-dashboard/compare/x...main");
    expect(screen.queryByText("Checking...")).not.toBeInTheDocument();
  });

  it.each([
    ["main identical", build({ divergence: { base: "main", status: "identical", behind: 0, ahead: 0 } }), "Up to date with main"],
    ["main 1 behind", build({ divergence: { base: "main", status: "behind", behind: 1, ahead: 0 } }), "1 commit behind main"],
    ["main ahead", build({ divergence: { base: "main", status: "ahead", behind: 0, ahead: 2 } }), "Not on main"],
    ["main diverged", build({ divergence: { base: "main", status: "diverged", behind: 4, ahead: 2 } }), "Not on main"],
    ["pr identical", build({ running: "pr", pr: 5123, divergence: { base: "pr-5123", status: "identical", behind: 0, ahead: 0, prState: "open" } }), "Latest commit of PR #5123"],
    ["pr behind", build({ running: "pr", pr: 5123, divergence: { base: "pr-5123", status: "behind", behind: 2, ahead: 0, prState: "open" } }), "PR #5123 has 2 newer commits"],
    ["pr merged", build({ running: "pr", pr: 5123, divergence: { base: "pr-5123", status: "identical", behind: 0, ahead: 0, prState: "merged" } }), "PR #5123 merged"],
    ["unknown", build({ divergence: { base: "main", status: "unknown", behind: 0, ahead: 0, reason: "github_error", message: "GitHub compare returned 502" } }), "Distance from main unknown"],
    ["no commit", build({ commit: undefined, commitURL: undefined, reason: "no_commit_label", message: "The image has no labels." }), "Commit unknown"],
  ])("labels the divergence: %s", (_name, b, text) => {
    render(<DashboardImages images={[image("rhods-dashboard", { running: b.running, runningPR: b.pr, flavor: "rhoai" })]} defaultExpanded buildDetails={details([b])} />);
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  it("offers Find in nightlies for a PR build with a commit, not for a build without one", () => {
    // Without a commit URL the link carries the bare SHA.
    render(<DashboardImages images={[image("rhods-dashboard", { running: "pr", runningPR: 5123, flavor: "rhoai" })]} defaultExpanded
      buildDetails={details([build({ running: "pr", pr: 5123, buildDate: undefined, commitURL: undefined, divergence: { base: "pr-5123", status: "identical", behind: 0, ahead: 0, prState: "open" } })])} />);
    expect(screen.getByRole("link", { name: "Find commit 21c06a3 in nightlies" })).toHaveAttribute("href", `/builds?q=${SHA}`);
    render(<DashboardImages images={[image("notebooks-ui", { running: "main", flavor: "rhoai" })]} defaultExpanded
      buildDetails={details([build({ deployment: "notebooks-ui", container: "notebooks-ui", commit: undefined, commitURL: undefined, reason: "no_commit_label", message: "The image has no labels." })])} />);
    expect(screen.getAllByRole("link", { name: /Find commit/ })).toHaveLength(1);
  });

  it("ignores details that describe another image than the one running", () => {
    const other = `quay.io/opendatahub/odh-dashboard:main-0000000@sha256:${"b".repeat(64)}`;
    render(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" })]} defaultExpanded
      buildDetails={details([build({ image: other, divergence: { base: "main", status: "identical", behind: 0, ahead: 0 } })])} />);
    expect(screen.getByText("21c06a3")).toBeInTheDocument(); // the tag, shown short
    expect(screen.queryByRole("link", { name: "21c06a3" })).not.toBeInTheDocument();
    expect(screen.queryByText("Up to date with main")).not.toBeInTheDocument();
  });

  it("says when GitHub is rate limited and a newer build exists", () => {
    const newerSHA = "3".repeat(40);
    render(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" })]} defaultExpanded
      buildDetails={details([build({
        divergence: { base: "main", status: "unknown", behind: 0, ahead: 0, reason: "rate_limited", message: "GitHub's API rate limit is reached." },
        newer: { image: `quay.io/opendatahub/odh-dashboard:${newerSHA}@sha256:${"3".repeat(64)}`, tag: "odh-stable", commit: newerSHA, commitURL: `https://github.com/opendatahub-io/odh-dashboard/commit/${newerSHA}`, buildDate: new Date(Date.now() - 600_000).toISOString() },
      })], { rateLimited: true, retryAfterSeconds: 900 })} />);
    expect(screen.getByText("Distance from main unknown")).toBeInTheDocument();
    expect(screen.getByText(/GitHub's rate limit is reached/)).toHaveTextContent("15 min");
    expect(screen.getByText(/Newer build available:/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "3333333" })).toHaveAttribute("href", `https://github.com/opendatahub-io/odh-dashboard/commit/${newerSHA}`);
    expect(screen.getByText("built 10m ago")).toBeInTheDocument();
  });

  it("shows a spinner while the details load and an inline note when they fail", () => {
    const { rerender } = render(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" }), image("notebooks-ui", { running: "release", currentImage: "r", defaultImage: "r" })]} defaultExpanded
      buildDetails={details([], { loading: true })} />);
    expect(screen.getAllByText("Checking...")).toHaveLength(1);
    rerender(<DashboardImages images={[image("rhods-dashboard", { running: "main", flavor: "rhoai" })]} defaultExpanded
      buildDetails={{ builds: [], loading: false, rateLimited: false, error: { status: 500, errorCode: "internal", message: "boom" } as never }} />);
    expect(screen.queryByText("Checking...")).not.toBeInTheDocument();
    expect(screen.getByText(/Build details could not be loaded/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
