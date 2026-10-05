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
    expect(await screen.findByText(/Installed build:/)).toBeInTheDocument();
    expect(screen.getByText("rhoai-3.6 @ 4444444")).toBeInTheDocument();
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
    expect(await screen.findByText("1 repositories with new commits")).toBeInTheDocument();
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
    expect(await screen.findByText("The installed build is unknown")).toBeInTheDocument();
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

  it("explains that PR numbers cannot be searched instead of failing", async () => {
    const api = setup();
    renderPage();
    await screen.findByRole("grid", { name: "Nightly builds" });
    const box = screen.getByRole("textbox", { name: /Find a build/ });
    fireEvent.change(box, { target: { value: "#10085" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(await screen.findByText("Search by PR number (#10085) is not available")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.includes("/content"))).toEqual([]);
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
    const all = screen.getByRole("button", { name: "All (1)" });
    expect(all).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "Core (1)" })).toHaveAttribute("aria-pressed", "true");
  });

  it("a Quay error is classified, never shown as a missing pull secret (A08-6)", async () => {
    stubApi({ "/api/build-explorer/tags": () => jsonResponse({ error: "Quay token request returned HTTP 401", errorCode: "registry_auth" }, 502) });
    renderPage();
    expect(await screen.findByText("Quay rejected the pull secret")).toBeInTheDocument();
    expect(screen.queryByText(/Pull secret not configured/)).not.toBeInTheDocument();
  });
});
