import React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import type { FBCContentResponse, RelatedImage, StatusResponse } from "../types";
import { stubApi, jsonResponse, type Handler } from "../test/apiStub";
import { deferred } from "../test/utils";

const statusRef: { current: Partial<StatusResponse> | null } = { current: null };
vi.mock("../state/AppState", () => ({
  useClusterStatus: () => ({ status: statusRef.current, loading: false, error: null, lastRefreshed: null, refresh: async () => {} }),
}));

// The app-wide notices need the AppInfo provider; they are tested on their own.
vi.mock("../components/GlobalBanners", () => ({ GlobalBanners: () => null }));

const { BuildExplorerPage } = await import("./BuildExplorerPage");

const FBC = "quay.io/rhoai/rhoai-fbc-fragment";
const INSTALLED = `${FBC}:rhoai-3.6@sha256:${"4".repeat(64)}`;
const LATEST = `${FBC}:rhoai-3.6@sha256:${"0".repeat(64)}`;
const EA = `${FBC}:rhoai-3.6-ea.2@sha256:${"b".repeat(64)}`;
const R35 = `${FBC}:rhoai-3.5@sha256:${"2".repeat(64)}`;

function ri(name: string, image: string, gitCommit?: string): RelatedImage {
  return { name, image, category: "core", gitCommit, gitURL: gitCommit ? "https://github.com/red-hat-data-services/odh-dashboard" : undefined };
}

const contents: Record<string, RelatedImage[]> = {
  [INSTALLED]: [ri("odh_dashboard_image", "d@sha256:1", "32a213123fbc"), ri("odh_kserve_image", "k@sha256:1", "kkkkkkk1")],
  [LATEST]: [ri("odh_dashboard_image", "d@sha256:2", "a3b6b581aaaa"), ri("odh_kserve_image", "k@sha256:1", "kkkkkkk1")],
  [EA]: [ri("odh_dashboard_image", "d@sha256:3", "32a213123fbc")],
  [R35]: [ri("odh_dashboard_image", "d@sha256:4", "eeeeeee1")],
};

function contentHandler(opts: { labels?: Promise<void> } = {}): Handler {
  return async (url) => {
    const params = new URL(url, "http://x").searchParams;
    const image = params.get("image")!;
    const withLabels = params.get("labels") === "true";
    if (withLabels && opts.labels) await opts.labels;
    const imgs = (contents[image] ?? []).map((i) => (withLabels ? i : { ...i, gitCommit: undefined, gitURL: undefined }));
    const body: FBCContentResponse = { tag: "", image, bundleName: "rhods-operator.3.6.0", relatedImages: imgs, categories: { core: imgs.length } };
    return jsonResponse(body);
  };
}

function setup(extra: Record<string, Handler | unknown> = {}) {
  return stubApi({
    "/api/build-explorer/tags": { tags: [{ tag: "rhoai-3.6", image: LATEST }, { tag: "rhoai-3.6-ea.2", image: EA }, { tag: "rhoai-3.5", image: R35 }] },
    "/api/build-explorer/content": contentHandler(),
    ...extra,
  });
}

function renderPage() {
  return render(<MemoryRouter initialEntries={["/builds"]}><BuildExplorerPage /></MemoryRouter>);
}

function tagRow(tag: string): HTMLElement {
  const table = screen.getByRole("grid", { name: "Nightly builds" });
  return within(table).getAllByRole("row").find((r) => r.querySelector('td[data-label="Tag"] strong')?.textContent === tag)!;
}

/** Types a search and presses Enter. */
async function search(query: string) {
  await screen.findByRole("grid", { name: "Nightly builds" });
  const box = screen.getByRole("textbox", { name: /Find a build/ });
  fireEvent.change(box, { target: { value: query } });
  fireEvent.keyDown(box, { key: "Enter" });
}

beforeEach(() => {
  statusRef.current = {
    subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "stable-3.x", state: "AtLatestKnown" },
    catalogSource: { exists: true, name: "rhoai-catalog-dev", image: INSTALLED, state: "READY" },
    nightly: {
      installed: { image: INSTALLED, tag: "rhoai-3.6", digest: `sha256:${"4".repeat(64)}`, buildDate: "2026-10-01T12:00:00Z" },
      latest: { image: LATEST, tag: "rhoai-3.6", digest: `sha256:${"0".repeat(64)}`, buildDate: "2026-10-05T12:00:00Z" },
      updateAvailable: true,
    },
  };
});

describe("Build Explorer installed build and compare (A08-5)", () => {
  it("names the installed build and marks the newer build of its stream", async () => {
    setup();
    renderPage();
    const installedBuild = (await screen.findByText("Installed build")).closest(".pf-v6-c-description-list__group") as HTMLElement;
    expect(within(installedBuild).getByText("rhoai-3.6")).toBeInTheDocument();
    expect(within(installedBuild).getByText("444444444444")).toBeInTheDocument();
    await screen.findByRole("grid", { name: "Nightly builds" });
    expect(within(tagRow("rhoai-3.6")).getByText("Newer than installed")).toBeInTheDocument();
    expect(within(tagRow("rhoai-3.5")).queryByText("Newer than installed")).not.toBeInTheDocument();
  });

  it("marks the row whose digest is the installed one", async () => {
    statusRef.current!.nightly = { installed: { image: LATEST, tag: "rhoai-3.6", digest: `sha256:${"0".repeat(64)}` }, updateAvailable: false };
    setup();
    renderPage();
    await screen.findByRole("grid", { name: "Nightly builds" });
    expect(within(tagRow("rhoai-3.6")).getByText("Installed")).toBeInTheDocument();
    expect(within(tagRow("rhoai-3.6")).queryByRole("button", { name: /Compare/ })).not.toBeInTheDocument();
  });

  it("compares the installed build with the latest: repositories, commits and a GitHub compare link", async () => {
    setup();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Compare with the latest rhoai-3.6" }));
    expect(await screen.findByText("1 repository with new commits")).toBeInTheDocument();
    expect(screen.getByText("1 unchanged")).toBeInTheDocument();
    const table = screen.getByRole("grid", { name: "Repositories that differ" });
    expect(within(table).getByText("red-hat-data-services/odh-dashboard")).toBeInTheDocument();
    expect(within(table).getByRole("link", { name: /Compare red-hat-data-services\/odh-dashboard/ }))
      .toHaveAttribute("href", "https://github.com/red-hat-data-services/odh-dashboard/compare/32a213123fbc...a3b6b581aaaa");
    fireEvent.click(screen.getByRole("button", { name: "Close comparison" }));
    expect(screen.queryByRole("grid", { name: "Repositories that differ" })).not.toBeInTheDocument();
  });

  it("without a nightly install there is nothing to compare with", async () => {
    statusRef.current = { subscription: { name: "rhods-operator", source: "redhat-operators", channel: "stable", state: "" }, catalogSource: { exists: false, name: "", image: "", state: "" } };
    setup();
    renderPage();
    expect(await screen.findByText(/was not installed from a nightly build/)).toBeInTheDocument();
    await screen.findByRole("grid", { name: "Nightly builds" });
    expect(screen.queryByRole("button", { name: /Compare .* with the installed build/ })).not.toBeInTheDocument();
  });
});

describe("Build Explorer search", () => {
  const merge = "fa1bcdbf5d61a7efd4b83f3c36e9d61f591326b3";
  const containsCalls = (api: { calls: string[] }) => api.calls.filter((c) => c.includes("/contains"));
  /** A PR answer for one build, as the backend sends it. */
  const prAnswer = (url: string, result: string, commit: string, extra: Record<string, unknown> = {}) => {
    const params = new URL(url, "http://x").searchParams;
    const image = params.get("image")!;
    const repo = params.get("repo") ?? "opendatahub-io/odh-dashboard";
    return jsonResponse({
      repo, pr: Number(params.get("pr")), title: "Fix the thing", url: `https://github.com/${repo}/pull/${params.get("pr")}`,
      mergeCommit: merge, mergedAt: "2026-10-05T21:16:13Z",
      builds: [{ image, result, commit, commitRepo: "red-hat-data-services/odh-dashboard", compareURL: `https://github.com/red-hat-data-services/odh-dashboard/compare/${merge}...${commit}`, ...extra }],
    });
  };

  it("a commit GitHub does not know falls back to the builds with a component built from exactly that commit", async () => {
    const api = setup({
      "/api/build-explorer/contains": () => jsonResponse({ error: "GitHub does not know commit 32a2131 in opendatahub-io/odh-dashboard or red-hat-data-services/odh-dashboard.", errorCode: "commit_not_found" }, 404),
    });
    renderPage();
    await search("32a2131");
    expect(await screen.findByText(/Checked 4 builds:/)).toBeInTheDocument();
    // One containment request (the first build alone), then the label scan.
    expect(containsCalls(api)).toHaveLength(1);
    expect(containsCalls(api)[0]).toContain("commit=32a2131");
    expect(screen.getByText(/GitHub does not know/)).toHaveTextContent(
      "GitHub does not know 32a2131 in opendatahub-io/odh-dashboard or red-hat-data-services/odh-dashboard, so the builds were searched for a component image built from exactly this commit.");
    expect(screen.getByText(/Checked 4 builds:/)).toHaveTextContent("Builds that are not listed have no component built from exactly this commit (a later build of the same branch is not listed).");
    const table = screen.getByRole("grid", { name: /Builds with a component built from 32a2131/ });
    const builds = within(table).getAllByRole("row").slice(1).map((r) => r.querySelector("strong")?.textContent);
    expect(builds).toEqual(["Installed (rhoai-3.6 @ 4444444)", "rhoai-3.6-ea.2"]);
  });

  it("a commit GitHub knows is searched by containment, with the commit's title, date and link", async () => {
    const sha = "6c8ac6e0f1d2c3b4a5968778695a4b3c2d1e0f9a";
    const build = "a3b6b581b465f97f8701f220efadcf8787879dde";
    const verdicts: Record<string, string> = { [INSTALLED]: "not_contained", [LATEST]: "contains", [EA]: "not_contained", [R35]: "contains" };
    const api = setup({
      "/api/build-explorer/contains": (url: string) => {
        const image = new URL(url, "http://x").searchParams.get("image")!;
        return jsonResponse({
          repo: "opendatahub-io/odh-dashboard", commit: sha, commitDate: "2026-10-05T10:00:00Z", commitTitle: "Show the model deployment status",
          commitURL: `https://github.com/opendatahub-io/odh-dashboard/commit/${sha}`,
          builds: [{ image, result: verdicts[image], commit: build, commitRepo: "red-hat-data-services/odh-dashboard", compareURL: `https://github.com/red-hat-data-services/odh-dashboard/compare/${sha}...${build}` }],
        });
      },
    });
    renderPage();
    await search("6C8AC6E");
    expect(await screen.findByText("Commit 6c8ac6e is in rhoai-3.6 and rhoai-3.5.")).toBeInTheDocument();
    expect(screen.getByText("Not in the installed build and rhoai-3.6-ea.2.")).toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(4);
    expect(containsCalls(api).every((c) => c.includes("commit=6c8ac6e") && !c.includes("repo="))).toBe(true);
    expect(screen.getByRole("link", { name: /^Commit 6c8ac6e$/ })).toHaveAttribute("href", `https://github.com/opendatahub-io/odh-dashboard/commit/${sha}`);
    // The title is on the header line, not in the sentence.
    expect(screen.getByText(/Committed/).textContent).toMatch(/^Commit 6c8ac6e: Show the model deployment status\. Committed 3w ago .* in opendatahub-io\/odh-dashboard\.$/);
    const table = screen.getByRole("grid", { name: "Builds and Commit 6c8ac6e" });
    expect(within(table).getByRole("columnheader", { name: /Commit 6c8ac6e/ })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: "Commit in build" })).toBeInTheDocument();
    expect(within(table).getAllByText("Contains 6c8ac6e")).toHaveLength(2);
    fireEvent.click(within(table).getByRole("button", { name: "Actions for rhoai-3.6" }));
    expect(screen.getByRole("menuitem", { name: "GitHub comparison of commit 6c8ac6e with rhoai-3.6" })).toHaveAttribute("href", `https://github.com/red-hat-data-services/odh-dashboard/compare/${sha}...${build}`);
  });

  it.each(["#9938", "PR 9938", "pr#9938", "9938"])("%s searches which builds contain the merged PR, with links and progress", async (query) => {
    const verdicts: Record<string, { result: string; commit: string }> = {
      [INSTALLED]: { result: "not_contained", commit: "32a213123fbc434a602ba5b05e4a2022dd0d85d6" },
      [LATEST]: { result: "contains", commit: "a3b6b581b465f97f8701f220efadcf8787879dde" },
      [EA]: { result: "not_contained", commit: "32a213123fbc434a602ba5b05e4a2022dd0d85d6" },
      [R35]: { result: "contains", commit: "eeeeeee1eeeeeee1eeeeeee1eeeeeee1eeeeeee1" },
    };
    const first = deferred<void>();
    const gate = deferred<void>();
    const api = setup({
      "/api/build-explorer/contains": async (url: string) => {
        const image = new URL(url, "http://x").searchParams.get("image")!;
        await (image === INSTALLED ? first.promise : gate.promise);
        return prAnswer(url, verdicts[image].result, verdicts[image].commit);
      },
    });
    renderPage();
    await search(query);
    // The PR is resolved with the first build alone, then the rest go a few at a time.
    expect(await screen.findByText(/Checked 0 of 4 builds for PR #9938/)).toBeInTheDocument();
    await waitFor(() => expect(containsCalls(api)).toHaveLength(1));
    await new Promise((r) => setTimeout(r, 20));
    expect(containsCalls(api)).toHaveLength(1);
    first.resolve();
    expect(await screen.findByText(/Checked 1 of 4 builds for PR #9938/)).toBeInTheDocument();
    await waitFor(() => expect(containsCalls(api)).toHaveLength(4));
    gate.resolve();
    expect(await screen.findByText("PR #9938 is in rhoai-3.6 and rhoai-3.5.")).toBeInTheDocument();
    expect(screen.getByText("Not in the installed build and rhoai-3.6-ea.2.")).toBeInTheDocument();
    const calls = containsCalls(api);
    expect(calls).toHaveLength(4);
    expect(calls.every((c) => c.startsWith("GET ") && c.includes("pr=9938") && !c.includes("repo="))).toBe(true);
    expect(screen.getByRole("link", { name: /^PR #9938$/ })).toHaveAttribute("href", "https://github.com/opendatahub-io/odh-dashboard/pull/9938");
    expect(screen.getByText("fa1bcdbf5d61")).toBeInTheDocument();
    const table = screen.getByRole("grid", { name: "Builds and PR #9938" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => [r.querySelector("strong")?.textContent, r.querySelector(".pf-v6-c-label")?.textContent])).toEqual([
      ["Installed (rhoai-3.6 @ 4444444)", "Does not contain"],
      ["rhoai-3.6", "Contains PR #9938"],
      ["rhoai-3.6-ea.2", "Does not contain"],
      ["rhoai-3.5", "Contains PR #9938"],
    ]);
    expect(within(rows[1]).getByRole("link", { name: "a3b6b58" })).toHaveAttribute("href", "https://github.com/red-hat-data-services/odh-dashboard/commit/a3b6b581b465f97f8701f220efadcf8787879dde");
    // The row actions are in a menu: the installed build is never offered "Compare with installed".
    fireEvent.click(within(rows[0]).getByRole("button", { name: /Actions for Installed/ }));
    expect(screen.queryByRole("menuitem", { name: "Compare with installed" })).not.toBeInTheDocument();
    fireEvent.click(within(rows[0]).getByRole("button", { name: /Actions for Installed/ }));
    fireEvent.click(within(rows[1]).getByRole("button", { name: "Actions for rhoai-3.6" }));
    expect(screen.getByRole("menuitem", { name: /GitHub comparison of PR #9938's merge commit with rhoai-3.6/ }))
      .toHaveAttribute("href", `https://github.com/red-hat-data-services/odh-dashboard/compare/${merge}...a3b6b581b465f97f8701f220efadcf8787879dde`);
    expect(screen.getByRole("menuitem", { name: "Compare with installed" })).toBeInTheDocument();
  });

  it("a PR URL of another component repository is searched in that repository", async () => {
    const api = setup({ "/api/build-explorer/contains": (url: string) => prAnswer(url, "contains", "c".repeat(40)) });
    renderPage();
    await search("https://github.com/opendatahub-io/data-science-pipelines/pull/42/files");
    expect(await screen.findByText("PR #42 is in the installed build, rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(4);
    expect(containsCalls(api).every((c) => c.includes("pr=42") && c.includes("repo=opendatahub-io%2Fdata-science-pipelines"))).toBe(true);
    expect(screen.getByRole("link", { name: /^PR #42$/ })).toHaveAttribute("href", "https://github.com/opendatahub-io/data-science-pipelines/pull/42");
    const table = screen.getByRole("grid", { name: "Builds and PR #42" });
    expect(within(table).getByRole("columnheader", { name: "Commit in build" })).toBeInTheDocument();
    expect(within(table).queryByRole("columnheader", { name: /data-science-pipelines/ })).not.toBeInTheDocument();
  });

  it("a repository the search does not cover is refused with the server's message", async () => {
    const message = "PR and commit search cover repositories of opendatahub-io, red-hat-data-services, kubeflow, kserve, trustyai-explainability, llm-d, feast-dev and ray-project.";
    const api = setup({ "/api/build-explorer/contains": () => jsonResponse({ error: message, errorCode: "validation" }, 400) });
    renderPage();
    await search("https://github.com/evil/repo/pull/5");
    expect(await screen.findByText("Cannot search evil/repo for PR #5")).toBeInTheDocument();
    expect(screen.getByText(message)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(1);
  });

  it("builds built more than an hour before the merge are answered without a request and counted in the summary", async () => {
    // The merge is 2026-10-05T21:16:13Z. rhoai-3.6-ea.2 was built 2 hours before it and is skipped;
    // rhoai-3.5, 30 minutes before, is within the clock-skew margin and is asked; rhoai-3.6 is after it.
    const api = setup({
      "/api/build-explorer/tags": {
        tags: [
          { tag: "rhoai-3.6", image: LATEST, buildDate: "2026-10-06T02:00:00Z" },
          { tag: "rhoai-3.6-ea.2", image: EA, buildDate: "2026-10-05T19:16:13Z" },
          { tag: "rhoai-3.5", image: R35, buildDate: "2026-10-05T20:46:13Z" },
        ],
      },
      "/api/build-explorer/contains": (url: string) => {
        const image = new URL(url, "http://x").searchParams.get("image");
        if (image === INSTALLED) return prAnswer(url, "not_contained", "", { reason: "built_before_merge", message: "Built 4 days before the PR was merged.", compareURL: undefined, commit: undefined, commitRepo: undefined });
        return prAnswer(url, image === R35 ? "not_contained" : "contains", "a".repeat(40));
      },
    });
    renderPage();
    await search("#9938");
    expect(await screen.findByText("PR #9938 is in rhoai-3.6.")).toBeInTheDocument();
    expect(screen.getByText("Not in rhoai-3.5. 2 older builds were built before the merge.")).toBeInTheDocument();
    // The installed build (asked first; the server answers from its date), rhoai-3.6 and rhoai-3.5: three requests, none for rhoai-3.6-ea.2.
    const calls = containsCalls(api);
    expect(calls).toHaveLength(3);
    expect(calls.some((c) => c.includes(encodeURIComponent(EA)))).toBe(false);
    expect(calls.some((c) => c.includes(encodeURIComponent(R35)))).toBe(true);
    const table = screen.getByRole("grid", { name: "Builds and PR #9938" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector("strong")?.textContent)).toEqual(["Installed (rhoai-3.6 @ 4444444)", "rhoai-3.6", "rhoai-3.6-ea.2", "rhoai-3.5"]);
    expect(within(rows[2]).getByText("Does not contain")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Built before the merge")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Does not contain")).toBeInTheDocument();
    expect(within(rows[3]).queryByText("Built before the merge")).not.toBeInTheDocument();
    expect(within(rows[0]).getByText("Built before the merge")).toBeInTheDocument();
  });

  it("says when no build contains the PR because it was merged after the newest build", async () => {
    const hours = (n: number) => new Date(Date.now() - n * 3600_000).toISOString();
    statusRef.current!.nightly = { installed: { image: INSTALLED, tag: "rhoai-3.6", digest: `sha256:${"4".repeat(64)}`, buildDate: hours(30) }, updateAvailable: false };
    const api = setup({
      "/api/build-explorer/tags": { tags: [{ tag: "rhoai-3.6", image: LATEST, buildDate: hours(20) }, { tag: "rhoai-3.5", image: R35, buildDate: hours(40) }] },
      "/api/build-explorer/contains": (url: string) => jsonResponse({
        repo: "opendatahub-io/odh-dashboard", pr: 5, title: "Brand new", mergeCommit: merge, mergedAt: hours(2),
        builds: [{ image: new URL(url, "http://x").searchParams.get("image"), result: "not_contained", reason: "built_before_merge", message: "Built 28 hours before the PR was merged." }],
      }),
    });
    renderPage();
    await search("#5");
    // (Each time is followed by its absolute date for screen readers.)
    expect((await screen.findByText(/No build contains PR #5 yet/)).textContent).toMatch(/^No build contains PR #5 yet: it was merged 2h ago .*, after the newest build \(20h ago .*\)\.$/);
    expect(screen.getByText("3 older builds were built before the merge.")).toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(1);
  });

  it("never offers to compare the installed build with itself when it is one of the listed tags", async () => {
    // The installed build is the newest rhoai-3.6, so it is listed under its tag, not as "Installed (...)".
    statusRef.current!.nightly = { installed: { image: LATEST, tag: "rhoai-3.6", digest: `sha256:${"0".repeat(64)}` }, updateAvailable: false };
    setup({
      "/api/build-explorer/contains": async (url: string) => {
        const image = new URL(url, "http://x").searchParams.get("image")!;
        return jsonResponse({ repo: "opendatahub-io/odh-dashboard", pr: 1, mergeCommit: "f".repeat(40), builds: [{ image, result: "contains" }] });
      },
    });
    renderPage();
    await search("#1");
    expect(await screen.findByText("PR #1 is in rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    // No GitHub link and no compare: the installed row has no actions at all.
    expect(screen.queryByRole("button", { name: "Actions for rhoai-3.6" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Actions for rhoai-3.5" }));
    expect(screen.getByRole("menuitem", { name: "Compare with installed" })).toBeInTheDocument();
  });

  it("a PR that is not merged stops after one request and says so", async () => {
    const api = setup({
      "/api/build-explorer/contains": () => jsonResponse({ error: "PR #10089 is open, so no build contains it", errorCode: "pr_not_merged" }, 422),
    });
    renderPage();
    await search("PR #10089");
    expect(await screen.findByText("PR #10089 is not merged")).toBeInTheDocument();
    expect(screen.getByText(/PR #10089 is open, so no build contains it\./)).toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(1);
    expect(screen.queryByRole("grid", { name: /Builds and PR/ })).not.toBeInTheDocument();
  });

  it("a GitHub rate limit shows unknown builds, when to retry, and searches again on request", async () => {
    let limited = true;
    const api = setup({
      "/api/build-explorer/contains": (url: string) => {
        const image = new URL(url, "http://x").searchParams.get("image")!;
        return jsonResponse(limited
          ? { repo: "opendatahub-io/odh-dashboard", pr: 1, builds: [{ image, result: "unknown", reason: "rate_limited", commit: "32a213123fbc434a602ba5b05e4a2022dd0d85d6", commitRepo: "red-hat-data-services/odh-dashboard" }], rateLimited: true, retryAfterSeconds: 600 }
          : { repo: "opendatahub-io/odh-dashboard", pr: 1, mergeCommit: "f".repeat(40), builds: [{ image, result: "contains" }] });
      },
    });
    renderPage();
    await search("#1");
    expect(await screen.findByText("GitHub's rate limit was reached")).toBeInTheDocument();
    expect(screen.getByText(/Try again in about 10 minutes/)).toBeInTheDocument();
    // Nothing is known, so nothing is claimed.
    expect(screen.getByText("Could not tell whether any build contains PR #1.")).toBeInTheDocument();
    expect(screen.queryByText(/No build contains/)).not.toBeInTheDocument();
    expect(screen.getByText("4 builds could not be checked (GitHub rate limit).")).toBeInTheDocument();
    expect(screen.getAllByText("Unknown (GitHub rate limit)")).toHaveLength(4);
    limited = false;
    fireEvent.click(screen.getByRole("button", { name: "Search again" }));
    expect(await screen.findByText("PR #1 is in the installed build, rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    expect(screen.queryByText("GitHub's rate limit was reached")).not.toBeInTheDocument();
    expect(containsCalls(api)).toHaveLength(8);
  });
});

describe("Build Explorer search in the URL (?q=)", () => {
  const Location: React.FC = () => <output data-testid="location">{useLocation().search}</output>;
  const renderAt = (path: string) => render(<MemoryRouter initialEntries={[path]}><BuildExplorerPage /><Location /></MemoryRouter>);
  const prOK = (url: string) => {
    const params = new URL(url, "http://x").searchParams;
    return jsonResponse({ repo: "opendatahub-io/odh-dashboard", pr: Number(params.get("pr")), mergeCommit: "f".repeat(40), builds: [{ image: params.get("image"), result: "contains" }] });
  };

  it("runs the search in ?q= once the builds are known", async () => {
    const api = setup({ "/api/build-explorer/contains": prOK });
    renderAt("/builds?q=%235123");
    expect(await screen.findByText("PR #5123 is in the installed build, rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: /Find a build/ })).toHaveValue("#5123");
    const calls = api.calls.filter((c) => c.includes("/contains"));
    expect(calls).toHaveLength(4);
    expect(calls.every((c) => c.includes("pr=5123"))).toBe(true);
    // The URL is kept as it was.
    expect(screen.getByTestId("location")).toHaveTextContent("?q=%235123");
  });

  it("a later ?q= in the URL runs that search without a remount, and an emptied one clears it", async () => {
    const Nav: React.FC<{ to: string; name: string }> = ({ to, name }) => {
      const navigate = useNavigate();
      return <button onClick={() => navigate(to)}>{name}</button>;
    };
    const api = setup({ "/api/build-explorer/contains": prOK });
    render(
      <MemoryRouter initialEntries={["/builds?q=%2310329"]}>
        <BuildExplorerPage /><Location /><Nav to="/builds?q=%2310330" name="go to the other PR" /><Nav to="/builds" name="go to the list" />
      </MemoryRouter>,
    );
    expect(await screen.findByText("PR #10329 is in the installed build, rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "go to the other PR" }));
    expect(await screen.findByText("PR #10330 is in the installed build, rhoai-3.6, rhoai-3.6-ea.2 and rhoai-3.5.")).toBeInTheDocument();
    expect(screen.queryByText(/PR #10329 is in/)).not.toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: /Find a build/ })).toHaveValue("#10330");
    expect(screen.getByTestId("location")).toHaveTextContent("?q=%2310330");
    const calls = api.calls.filter((c) => c.includes("/contains"));
    expect(calls.filter((c) => c.includes("pr=10329"))).toHaveLength(4);
    expect(calls.filter((c) => c.includes("pr=10330"))).toHaveLength(4);
    // Each search ran exactly once.
    expect(calls).toHaveLength(8);
    fireEvent.click(screen.getByRole("button", { name: "go to the list" }));
    expect(await screen.findByRole("grid", { name: "Nightly builds" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: /Find a build/ })).toHaveValue("");
    expect(screen.queryByText(/PR #10330 is in/)).not.toBeInTheDocument();
  });

  it("submitting a search writes it to the URL, clearing removes it, and the older ?image= still works", async () => {
    setup({ "/api/build-explorer/contains": prOK });
    renderAt(`/builds?image=${encodeURIComponent(R35)}`);
    expect(await screen.findByRole("grid", { name: "Components in the searched build" })).toBeInTheDocument();
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    await waitFor(() => expect(box).toHaveValue(R35));
    fireEvent.change(box, { target: { value: "  #77 " } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText(/PR #77 is in/)).toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("?q=%2377");
    fireEvent.click(screen.getByRole("button", { name: "Clear the search and show all builds" }));
    expect(await screen.findByRole("grid", { name: "Nightly builds" })).toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("");
  });
});

describe("Build Explorer rows (A08-9, A08-12)", () => {
  it("shows the components at once and fills commits in when labels arrive", async () => {
    const labels = deferred<void>();
    setup({ "/api/build-explorer/content": contentHandler({ labels: labels.promise }) });
    renderPage();
    await screen.findByRole("grid", { name: "Nightly builds" });
    fireEvent.click(within(tagRow("rhoai-3.5")).getAllByRole("button")[0]);
    const table = await screen.findByRole("grid", { name: "Components in rhoai-3.5" });
    expect(within(table).getByText("odh_dashboard_image")).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: "Commit" })).toBeInTheDocument();
    expect(within(table).getByText("Loading commit of odh_dashboard_image")).toBeInTheDocument();
    labels.resolve();
    await waitFor(() => expect(within(table).getByText("eeeeeee")).toBeInTheDocument());
    // Core is the default category; the select says so, with its count.
    expect(screen.getByRole("button", { name: "Category for rhoai-3.5: Core" })).toHaveTextContent("1");
  });

  it("a Quay error is classified, never shown as a missing pull secret (A08-6)", async () => {
    stubApi({ "/api/build-explorer/tags": () => jsonResponse({ error: "Quay token request returned HTTP 401", errorCode: "registry_auth" }, 502) });
    renderPage();
    expect(await screen.findByText(/^Quay rejected the pull secret/)).toBeInTheDocument();
    expect(screen.queryByText(/Pull secret not configured/)).not.toBeInTheDocument();
  });
});
