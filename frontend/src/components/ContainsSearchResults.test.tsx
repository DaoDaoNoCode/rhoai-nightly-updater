import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ContainsSearchResults, detailLine, listNames, sentenceName } from "./ContainsSearchResults";
import type { ContainsSearchResult, ContainsSearchState } from "../hooks/useContainsSearch";
import type { PRContainsBuild } from "../types";

const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
const hours = (n: number) => new Date(Date.now() - n * 3600_000).toISOString();

function result(tag: string, answer: Partial<PRContainsBuild>, opts: { installed?: boolean; builtHoursAgo?: number } = {}): ContainsSearchResult {
  // The installed build may share its tag with a listed build; their digests differ.
  const image = `${FBC}:${tag}@sha256:${(opts.installed ? "4" : tag.replace(/[^0-9a-f]/g, "0")).padEnd(64, "0")}`;
  return {
    build: { image, label: opts.installed ? `Installed (${tag} @ 0000000)` : tag, buildDate: opts.builtHoursAgo === undefined ? undefined : hours(opts.builtHoursAgo) },
    answer: { image, result: "unknown", ...answer },
  };
}

const done = (results: ContainsSearchResult[], info: ContainsSearchState["info"] = { repo: "opendatahub-io/odh-dashboard", pr: 5123, mergedAt: hours(40), mergeCommit: "f".repeat(40) }): ContainsSearchState =>
  ({ running: false, checked: results.length, total: results.length, info, results, error: null, rateLimited: false, retryAfterSeconds: 0 });

function renderResults(search: ContainsSearchState, query: { kind: "pr" | "commit"; value: string; repo?: string } = { kind: "pr", value: "5123" }) {
  return render(<ContainsSearchResults query={query} search={search} plannedTotal={search.total} canCompare={false} isInstalledImage={() => false} onCompare={() => {}} onRetry={() => {}} />);
}

describe("the words of the answer", () => {
  it("lists up to four names, then counts the rest", () => {
    expect(listNames([])).toBe("");
    expect(listNames(["a"])).toBe("a");
    expect(listNames(["a", "b"])).toBe("a and b");
    expect(listNames(["a", "b", "c"])).toBe("a, b and c");
    expect(listNames(["a", "b", "c", "d"])).toBe("a, b, c and d");
    expect(listNames(["a", "b", "c", "d", "e"])).toBe("a, b and 3 more builds");
  });

  it("names the installed build as such; the table keeps its label", () => {
    expect(sentenceName({ image: `${FBC}:rhoai-3.6@sha256:${"4".repeat(64)}`, label: "Installed (rhoai-3.6 @ 4444444)" })).toBe("the installed build");
    expect(sentenceName({ image: `${FBC}:rhoai-3.5@sha256:${"2".repeat(64)}`, label: "rhoai-3.5" })).toBe("rhoai-3.5");
  });

  it("the detail line: short sentences for the checked builds, the older ones and the unknown ones", () => {
    const results = [
      result("rhoai-3.7-ea.1", { result: "contains" }),
      result("rhoai-3.6", { result: "not_contained" }, { installed: true }),
      result("rhoai-3.5", { result: "not_contained" }),
      result("rhoai-3.4", { result: "not_contained", reason: "built_before_merge" }),
      result("rhoai-3.3", { result: "not_contained", reason: "built_before_merge" }),
      result("rhoai-3.2", { result: "unknown", reason: "rate_limited" }),
      result("rhoai-3.1", { result: "unknown", reason: "commit_not_found" }),
      result("rhoai-3.0", { result: "unknown", reason: "rate_limited" }),
    ];
    expect(detailLine("pr", results, "odh-dashboard")).toBe(
      "Not in the installed build and rhoai-3.5. 2 older builds were built before the merge. 3 builds could not be checked (GitHub rate limit, commit not on GitHub).");
    // At most four names, then a count.
    expect(detailLine("pr", [...results.slice(1, 4), ...["rhoai-3.3", "rhoai-2.25", "a", "b", "c"].map((t) => result(t, { result: "not_contained" }))], "odh-dashboard")).toBe(
      "Not in the installed build, rhoai-3.5, rhoai-3.3, rhoai-2.25 and 3 other checked builds. 1 older build was built before the merge.");
    expect(detailLine("pr", ["rhoai-3.5", "rhoai-3.4", "rhoai-3.3", "rhoai-2.25", "x"].map((t) => result(t, { result: "not_contained" })), "odh-dashboard")).toBe(
      "Not in rhoai-3.5, rhoai-3.4, rhoai-3.3, rhoai-2.25 and 1 other checked build.");
    expect(detailLine("pr", ["rhoai-3.5", "rhoai-3.4", "rhoai-3.3", "rhoai-2.25"].map((t) => result(t, { result: "not_contained" })), "odh-dashboard")).toBe(
      "Not in rhoai-3.5, rhoai-3.4, rhoai-3.3 and rhoai-2.25.");
    expect(detailLine("commit", results.slice(3, 4), "odh-dashboard")).toBe("1 older build was built before the commit.");
    expect(detailLine("commit", results.slice(6, 7), "kserve")).toBe("1 build could not be checked (commit not on GitHub).");
    expect(detailLine("pr", [result("rhoai-3.6", { result: "unknown", reason: "no_component_image" })], "data-science-pipelines")).toBe(
      "1 build could not be checked (no data-science-pipelines image in this build).");
    expect(detailLine("pr", results.slice(0, 1), "odh-dashboard")).toBe("");
  });
});

describe("the answer sentence", () => {
  it("some builds contain the PR", () => {
    renderResults(done([
      result("rhoai-3.6", { result: "contains" }, { installed: true }),
      result("rhoai-3.6", { result: "contains" }),
      result("rhoai-3.5", { result: "not_contained" }),
      result("rhoai-3.4", { result: "not_contained", reason: "built_before_merge" }),
    ]));
    expect(screen.getByText("PR #5123 is in the installed build and rhoai-3.6.")).toBeInTheDocument();
    expect(screen.getByText("Not in rhoai-3.5. 1 older build was built before the merge.")).toBeInTheDocument();
    // The table keeps the installed build's label and says why rhoai-3.4 does not contain it.
    expect(screen.getByText("Installed (rhoai-3.6 @ 0000000)")).toBeInTheDocument();
    expect(screen.getByText("Built before the merge")).toBeInTheDocument();
    expect(screen.getAllByText("Contains PR #5123")).toHaveLength(2);
    expect(screen.queryByText(/Checked/)).not.toBeInTheDocument();
  });

  it("no build contains the PR because it was merged after the newest build", () => {
    renderResults(done([
      result("rhoai-3.6", { result: "not_contained", reason: "built_before_merge" }, { builtHoursAgo: 50 }),
      result("rhoai-3.5", { result: "not_contained", reason: "built_before_merge" }, { builtHoursAgo: 70 }),
    ], { repo: "opendatahub-io/odh-dashboard", pr: 5123, mergedAt: hours(3), mergeCommit: "f".repeat(40) }));
    expect(screen.getByText(/No build contains PR #5123 yet/).textContent).toMatch(/^No build contains PR #5123 yet: it was merged 3h ago .*, after the newest build \(2d ago .*\)\.$/);
    expect(screen.getByText("2 older builds were built before the merge.")).toBeInTheDocument();
  });

  it("does not claim 'after the newest build' when a build's date is unknown", () => {
    renderResults(done([
      result("rhoai-3.6", { result: "not_contained" }),
      result("rhoai-3.5", { result: "not_contained", reason: "built_before_merge" }, { builtHoursAgo: 70 }),
    ], { repo: "opendatahub-io/odh-dashboard", pr: 5123, mergedAt: hours(3), mergeCommit: "f".repeat(40) }));
    expect(screen.getByText("No build contains PR #5123.")).toBeInTheDocument();
    expect(screen.getByText("Not in rhoai-3.6. 1 older build was built before the merge.")).toBeInTheDocument();
  });

  it("no checked build contains the PR, and some could not be checked", () => {
    renderResults(done([
      result("rhoai-3.6", { result: "not_contained" }, { builtHoursAgo: 10 }),
      result("rhoai-3.5", { result: "unknown", reason: "commit_not_found", message: "GitHub does not know commit ffffffffffff in opendatahub-io/odh-dashboard or red-hat-data-services/odh-dashboard." }),
    ]));
    expect(screen.getByText("No checked build contains PR #5123.")).toBeInTheDocument();
    expect(screen.getByText("Not in rhoai-3.6. 1 build could not be checked (commit not on GitHub).")).toBeInTheDocument();
    expect(screen.getByText("Unknown (commit not on GitHub)")).toBeInTheDocument();
    expect(screen.getByText(/GitHub does not know commit ffffffffffff/)).toBeInTheDocument();
    expect(screen.queryByText(/cherry-pick/)).not.toBeInTheDocument();
  });

  it("could not tell when every build is unknown (a rate limit)", () => {
    renderResults(done([
      result("rhoai-3.6", { result: "unknown", reason: "rate_limited" }, { builtHoursAgo: 10 }),
      result("rhoai-3.5", { result: "unknown", reason: "rate_limited" }, { builtHoursAgo: 30 }),
    ], { repo: "opendatahub-io/odh-dashboard", pr: 5123 }));
    expect(screen.getByText("Could not tell whether any build contains PR #5123.")).toBeInTheDocument();
    expect(screen.getByText("2 builds could not be checked (GitHub rate limit).")).toBeInTheDocument();
    expect(screen.queryByText(/No build contains/)).not.toBeInTheDocument();
  });

  it("could not tell whether any build contains a commit", () => {
    renderResults(done([result("rhoai-3.6", { result: "unknown", reason: "build_unreadable", message: "Could not read the build's catalog." })],
      { repo: "opendatahub-io/odh-dashboard", commit: "6c8ac6e0f1d2c3b4a5968778695a4b3c2d1e0f9a" }), { kind: "commit", value: "6c8ac6e" });
    expect(screen.getByText("Could not tell whether any build contains commit 6c8ac6e.")).toBeInTheDocument();
    expect(screen.getByText("1 build could not be checked (build not readable).")).toBeInTheDocument();
  });

  it("a commit: the title on the header line, a short sentence, and more than four builds", () => {
    const sha = "6c8ac6e0f1d2c3b4a5968778695a4b3c2d1e0f9a";
    renderResults(done(
      ["rhoai-3.7-ea.1", "rhoai-3.6", "rhoai-3.6-ea.2", "rhoai-3.5", "rhoai-3.4"].map((tag) => result(tag, { result: "contains" })),
      { repo: "opendatahub-io/odh-dashboard", commit: sha, commitTitle: "Update team ownership for zaffre (#10329)", commitDate: hours(40), commitURL: `https://github.com/red-hat-data-services/odh-dashboard/commit/${sha}` },
    ), { kind: "commit", value: "6c8ac6e" });
    expect(screen.getByText("Commit 6c8ac6e is in rhoai-3.7-ea.1, rhoai-3.6 and 3 more builds.")).toBeInTheDocument();
    // Found on the fork: the header line carries the title, the date and the repository, and links there.
    expect(screen.getByText(/Committed/).textContent).toMatch(/^Commit 6c8ac6e: Update team ownership for zaffre \(#10329\)\. Committed 1d ago .* in red-hat-data-services\/odh-dashboard\.$/);
    expect(screen.getByRole("link", { name: /^Commit 6c8ac6e$/ })).toHaveAttribute("href", `https://github.com/red-hat-data-services/odh-dashboard/commit/${sha}`);
    expect(screen.getAllByText("Contains 6c8ac6e")).toHaveLength(5);
    expect(screen.getByRole("columnheader", { name: /^Commit 6c8ac6e/ })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Commit in build" })).toBeInTheDocument();
  });

  it("while running, the progress line", () => {
    renderResults({ ...done([result("rhoai-3.6", { result: "contains" })]), running: true, checked: 1, total: 25 });
    expect(screen.getByText(/Checked 1 of 25 builds for PR #5123/)).toBeInTheDocument();
    expect(screen.queryByText(/is in/)).not.toBeInTheDocument();
  });

  it("a PR of another repository names its component", () => {
    renderResults(done([result("rhoai-3.6", { result: "unknown", reason: "no_component_image", message: "This build has no image built from red-hat-data-services/kserve or kserve/kserve." })],
      { repo: "kserve/kserve", pr: 9, mergedAt: hours(40), mergeCommit: "f".repeat(40), url: "https://github.com/kserve/kserve/pull/9" }), { kind: "pr", value: "9", repo: "kserve/kserve" });
    expect(screen.getByText("1 build could not be checked (no kserve image in this build).")).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Commit in build" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^PR #9$/ })).toHaveAttribute("href", "https://github.com/kserve/kserve/pull/9");
  });

  it("a repository the search does not cover is a warning with the server's message, without Retry", () => {
    const message = "PR and commit search cover repositories of opendatahub-io, red-hat-data-services, kubeflow, kserve, trustyai-explainability, llm-d, feast-dev and ray-project.";
    renderResults({ ...done([]), error: { status: 400, errorCode: "validation", message } as ContainsSearchState["error"] }, { kind: "pr", value: "5", repo: "evil/repo" });
    const alert = screen.getByText("Cannot search evil/repo for PR #5").closest(".pf-v6-c-alert") as HTMLElement;
    expect(alert).toHaveClass("pf-m-warning");
    expect(alert).toHaveTextContent(message);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  });
});
