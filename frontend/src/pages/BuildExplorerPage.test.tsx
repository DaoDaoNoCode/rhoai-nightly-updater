import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
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
  it("finds the builds with a component built from a commit, including the installed one", async () => {
    setup();
    renderPage();
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: "32a2131" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText(/Checked 4 builds:/)).toBeInTheDocument();
    const table = screen.getByRole("grid", { name: /Builds with a component built from 32a2131/ });
    const builds = within(table).getAllByRole("row").slice(1).map((r) => r.querySelector("strong")?.textContent);
    expect(builds).toEqual(["Installed (rhoai-3.6 @ 4444444)", "rhoai-3.6-ea.2"]);
  });

  it.each(["#9938", "PR 9938", "pr#9938", "9938"])("%s searches which builds contain the merged PR, with links and progress", async (query) => {
    const merge = "fa1bcdbf5d61a7efd4b83f3c36e9d61f591326b3";
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
        const params = new URL(url, "http://x").searchParams;
        const image = params.get("image")!;
        await (image === INSTALLED ? first.promise : gate.promise);
        const v = verdicts[image];
        return jsonResponse({
          repo: "opendatahub-io/odh-dashboard", pr: Number(params.get("pr")), title: "Fix the thing", url: "https://github.com/opendatahub-io/odh-dashboard/pull/9938",
          mergeCommit: merge, mergedAt: "2026-10-05T21:16:13Z",
          builds: [{ image, result: v.result, commit: v.commit, commitRepo: "red-hat-data-services/odh-dashboard", compareURL: `https://github.com/red-hat-data-services/odh-dashboard/compare/${merge}...${v.commit}` }],
        });
      },
    });
    renderPage();
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: query } });
    fireEvent.keyDown(box, { key: "Enter" });
    // The PR is resolved with the first build alone, then the rest go a few at a time.
    expect(await screen.findByText(/Checked 0 of 4 builds for PR #9938/)).toBeInTheDocument();
    await waitFor(() => expect(api.calls.filter((c) => c.includes("/contains"))).toHaveLength(1));
    await new Promise((r) => setTimeout(r, 20));
    expect(api.calls.filter((c) => c.includes("/contains"))).toHaveLength(1);
    first.resolve();
    expect(await screen.findByText(/Checked 1 of 4 builds for PR #9938/)).toBeInTheDocument();
    await waitFor(() => expect(api.calls.filter((c) => c.includes("/contains"))).toHaveLength(4));
    gate.resolve();
    expect(await screen.findByText(/Checked 4 builds: 2 contain PR #9938/)).toBeInTheDocument();
    const calls = api.calls.filter((c) => c.includes("/contains"));
    expect(calls).toHaveLength(4);
    expect(calls.every((c) => c.startsWith("GET ") && c.includes("pr=9938"))).toBe(true);
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
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: "#1" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText(/Checked 3 builds: 3 contain PR #1/)).toBeInTheDocument();
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
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: "PR #10089" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText("PR #10089 is not merged")).toBeInTheDocument();
    expect(screen.getByText(/PR #10089 is open, so no build contains it\./)).toBeInTheDocument();
    expect(api.calls.filter((c) => c.includes("/contains"))).toHaveLength(1);
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
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: "#1" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText("GitHub's rate limit was reached")).toBeInTheDocument();
    expect(screen.getByText(/Try again in about 10 minutes/)).toBeInTheDocument();
    expect(screen.getByText(/Checked 4 builds: none contains PR #1, 4 unknown/)).toBeInTheDocument();
    expect(screen.getAllByText("Unknown (GitHub rate limit)")).toHaveLength(4);
    limited = false;
    fireEvent.click(screen.getByRole("button", { name: "Search again" }));
    expect(await screen.findByText(/Checked 4 builds: 4 contain PR #1/)).toBeInTheDocument();
    expect(screen.queryByText("GitHub's rate limit was reached")).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.includes("/contains"))).toHaveLength(8);
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
