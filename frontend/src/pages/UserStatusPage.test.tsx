import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { UserStatusPage } from "./UserStatusPage";
import { USER_PERMISSIONS, renderPage, settleApps } from "../test/providers";
import { jsonResponse } from "../test/apiStub";
import { stubUserApi, summary } from "../test/userFixtures";
import { contract } from "../test/contract";
import { useOperation } from "../state/AppState";

const Where: React.FC = () => <span data-testid="where">{useLocation().pathname}</span>;
/** Asks GET /api/operation now, as the app's poller does on its schedule. */
const PollOperation: React.FC = () => <button type="button" onClick={useOperation().refreshServerOperation}>Poll operation</button>;

afterEach(() => {
  vi.unstubAllGlobals();
});

const render = () => renderPage(<UserStatusPage />, "/", { permissions: async () => USER_PERMISSIONS });
const card = (title: string) => screen.getByRole("heading", { name: title }).closest(".pf-v6-c-card") as HTMLElement;
const row = (scope: HTMLElement, term: string) => within(scope).getByText(term).closest(".pf-v6-c-description-list__group") as HTMLElement;

describe("Status for a user who is not a cluster-admin", () => {
  it("shows the summary: updater, RHOAI, nightly build and the DataScienceCluster", async () => {
    const api = stubUserApi();
    render();
    await settleApps();
    expect(await screen.findByRole("heading", { name: "RHOAI" })).toBeInTheDocument();
    // The values of the generated contract fixture (pkg/api TestFrontendContract).
    expect(within(row(document.body, "Updater version")).getByText(contract.summary().updaterVersion)).toBeInTheDocument();

    const rhoai = card("RHOAI");
    expect(within(row(rhoai, "Operator")).getByText("Succeeded")).toBeInTheDocument();
    expect(within(row(rhoai, "Version")).getByText("3.6.0")).toBeInTheDocument();
    expect(within(row(rhoai, "Channel")).getByText("beta")).toBeInTheDocument();

    const nightly = card("Nightly build");
    expect(within(row(nightly, "Installed")).getByText("rhoai-3.6")).toBeInTheDocument();
    expect(within(row(nightly, "Installed")).getByText(/^built /)).toBeInTheDocument();
    expect(within(row(nightly, "Newer build")).getByText("A newer rhoai-3.6 build exists")).toBeInTheDocument();

    const dsc = card("DataScienceCluster");
    expect(within(dsc.querySelector(".pf-v6-c-card__header") as HTMLElement).getByText("Not ready")).toBeInTheDocument();
    const rows = within(within(dsc).getByRole("grid", { name: "DataScienceCluster components" })).getAllByRole("row").slice(1).map((r) => r.textContent);
    expect(rows).toEqual(["dashboardManagedReady", "kserveManagedNot ready", "rayRemovedNot run by RHOAI"]);

    // Only the summary: no status, activity or nightly lookups.
    expect(api.calls.filter((c) => /\/api\/(status|activity|latest-nightly|nightly-tags)/.test(c))).toEqual([]);
    await settleApps();
  });

  it("has nothing to change: no update, reinstall, pull secret, activity or Argo CD", async () => {
    stubUserApi();
    render();
    await settleApps();
    await screen.findByRole("heading", { name: "RHOAI" });
    for (const text of [/Update to/, /Re-deploy/, /Install a different build/, /Pull secret/i, /Activity/, /Argo CD/, /Recovery/]) {
      expect(screen.queryByText(text)).not.toBeInTheDocument();
    }
    expect(screen.queryAllByRole("button").map((b) => b.textContent)).toEqual(expect.not.arrayContaining(["Update", "Reinstall"]));
    await settleApps();
  });

  it("says the newest build is installed", async () => {
    const s = contract.summary();
    stubUserApi({ "/api/summary": summary({ nightly: { ...s.nightly, latestBuild: s.nightly.installedBuild, newerAvailable: false } }) });
    render();
    await settleApps();
    expect(await screen.findByText("This is the newest build")).toBeInTheDocument();
    await settleApps();
  });

  it("says when it could not check for a newer build, never that this is the newest", async () => {
    const s = contract.summary();
    stubUserApi({ "/api/summary": summary({ nightly: { ...s.nightly, latestBuild: null, newerAvailable: false, problem: "lookup_failed" } }) });
    render();
    await settleApps();
    expect(await screen.findByText("Could not check for a newer build.")).toBeInTheDocument();
    expect(screen.queryByText("This is the newest build")).not.toBeInTheDocument();
    await settleApps();
  });

  it("an unreadable catalog is not a cluster without a nightly build", async () => {
    stubUserApi({ "/api/summary": summary({ nightly: { installedBuild: null, latestBuild: null, newerAvailable: false, problem: "catalog_unreadable" } }) });
    render();
    await settleApps();
    expect(await screen.findByText("Could not read which nightly build is installed.")).toBeInTheDocument();
    expect(screen.queryByText(/was not installed from a nightly build/)).not.toBeInTheDocument();
    await settleApps();
  });

  it("without RHOAI, a nightly build or a DataScienceCluster says so", async () => {
    stubUserApi({ "/api/summary": summary({
      rhoai: { installed: false, version: "", channel: "", phase: "" },
      nightly: { installedBuild: null, latestBuild: null, newerAvailable: false, problem: "" },
      dsc: { exists: false, ready: false, components: null },
    }) });
    render();
    await settleApps();
    expect(await screen.findByText("Not installed")).toBeInTheDocument();
    expect(screen.getByText("RHOAI on this cluster was not installed from a nightly build.")).toBeInTheDocument();
    expect(screen.getByText("There is no DataScienceCluster. A cluster-admin creates it.")).toBeInTheDocument();
    await settleApps();
  });

  it("opens the Build Explorer from the nightly build", async () => {
    stubUserApi();
    renderPage(<><UserStatusPage /><Where /></>, "/", { permissions: async () => USER_PERMISSIONS });
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Open Build Explorer" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/builds");
    await settleApps();
  });

  it("a summary that cannot be read shows Retry, without Diagnostics", async () => {
    stubUserApi({ "/api/summary": () => jsonResponse({ error: "The cluster API did not answer", errorCode: "cluster_unavailable" }, 503) });
    render();
    await settleApps();
    expect(await screen.findByRole("heading", { name: "Can't load the cluster summary" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open Diagnostics" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("a summary the user may not read says so, not Access denied", async () => {
    stubUserApi({ "/api/summary": () => jsonResponse({ error: "no", errorCode: "forbidden_for_user" }, 403) });
    render();
    await settleApps();
    expect(await screen.findByRole("heading", { name: "You cannot see this" })).toBeInTheDocument();
    expect(screen.queryByText(/Access denied/)).not.toBeInTheDocument();
    expect(screen.queryByText(/make upgrade/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("reads the summary again when the running update ends", async () => {
    let running = true;
    const api = stubUserApi();
    renderPage(<><UserStatusPage /><PollOperation /></>, "/", {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => (running ? { running: true, label: "Update to nightly", startedAt: "2026-10-06T14:05:00Z" } : { running: false }),
    });
    await settleApps();
    await screen.findByRole("heading", { name: "RHOAI" });
    expect(await screen.findByText(/A cluster operation is running since/)).toBeInTheDocument();
    const reads = () => api.calls.filter((c) => c.includes("/api/summary")).length;
    const before = reads();
    running = false;
    fireEvent.click(screen.getByRole("button", { name: "Poll operation" }));
    await settleApps();
    expect(screen.queryByText(/A cluster operation is running since/)).not.toBeInTheDocument();
    await waitFor(() => expect(reads()).toBe(before + 1));
  });
});
