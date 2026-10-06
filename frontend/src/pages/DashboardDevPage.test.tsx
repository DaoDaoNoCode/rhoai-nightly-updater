import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DashboardDevPage } from "./DashboardDevPage";
import { overrideAlerts } from "../components/DashboardSessionPanel";
import { stubApi, jsonResponse } from "../test/apiStub";
import { renderWithApp } from "../test/providers";
import type { DashboardDevImage, DashboardOverride, DashboardState } from "../types";

function image(container: string, over: Partial<DashboardDevImage> = {}): DashboardDevImage {
  return {
    deployment: container, container, envVar: "X", repository: `opendatahub/${container}`,
    currentImage: `registry.redhat.io/rhoai/${container}@sha256:${"1".repeat(64)}`,
    defaultImage: `registry.redhat.io/rhoai/${container}@sha256:${"1".repeat(64)}`,
    ready: true, matchesTarget: true, running: "release", ...over,
  };
}

function clean(over: Partial<DashboardState> = {}): DashboardState {
  return {
    operatorAvailable: true, operatorPaused: false, isDevMode: false, allDevImagesReady: true, devImagesMatchTarget: true,
    defaultImagesRestored: true, currentImage: "registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:1", deploymentMode: "Standalone",
    isCustomPR: false, managed: true, podStatus: "Running", podReady: true, containersReady: 3, containersTotal: 3,
    rolloutPending: false, canAssistRollout: false, dashboardURL: "https://dash.example",
    devImages: [image("rhods-dashboard"), image("notebooks-ui")],
    override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false, releaseVersion: "3.6.0" },
    defaultFlavor: "rhoai", availableFlavors: ["rhoai", "odh"], rolloutStuck: false,
    ...over,
  };
}

const session: DashboardOverride = {
  active: true, operatorPaused: true, sessionRecorded: true, mode: "pr", prNumber: 222, flavor: "rhoai",
  startedAt: "2026-10-03T14:05:00Z", startedBy: "alice", updatedAt: "2026-10-03T14:05:00Z", updatedBy: "alice",
  releaseVersionAtStart: "3.6.0", releaseVersion: "3.6.1", stale: true, staleReasons: ["RHOAI version changed from 3.6.0 to 3.6.1"],
  dashboardDeleting: false, warnings: ["RHOAI was updated while dashboard-operator is paused. Revert to default to apply the new release."],
  components: [{ deployment: "rhods-dashboard", container: "rhods-dashboard", source: "pr", tag: "odh-pr-222" }, { deployment: "notebooks-ui", container: "notebooks-ui", source: "baseline" }],
};

function active(over: Partial<DashboardState> = {}): DashboardState {
  return clean({
    operatorPaused: true, isDevMode: true, devMode: "pr", isCustomPR: true, prNumber: 222, flavor: "rhoai",
    devImages: [image("rhods-dashboard", { running: "pr", runningPR: 222, flavor: "rhoai", currentImage: `quay.io/opendatahub/odh-dashboard:odh-pr-222@sha256:${"2".repeat(64)}` }), image("notebooks-ui")],
    override: session,
    ...over,
  });
}

function renderPage(canMutate = true) {
  return renderWithApp(<DashboardDevPage />, { permissions: async () => ({ canMutate, user: "me" }) }, "/dashboard-dev");
}

describe("Dashboard Dev session panel (A04-1)", () => {
  it("shows who, since when, the build, every warning and a Revert that confirms what changes", async () => {
    const api = stubApi({
      "/api/dashboard/state": active(),
      "POST /api/dashboard/revert": { success: true, message: "Reverted", logs: [] },
      "/api/resources/status": { minio: { deployed: false, ready: false }, mlflow: { deployed: false, ready: false }, pipelineServers: [] },
    });
    renderPage();
    expect(await screen.findByText("Dashboard Dev session active: PR #222")).toBeInTheDocument();
    expect(screen.getByText("alice")).toBeInTheDocument();
    expect(screen.getAllByText("RHOAI build (Konflux)").length).toBeGreaterThan(0);
    expect(screen.getByText("RHOAI was updated while dashboard-operator is paused")).toBeInTheDocument();
    expect(screen.getByText("RHOAI version changed from 3.6.0 to 3.6.1")).toBeInTheDocument();
    expect(screen.getByText("1 of 2 run the PR build; 1 run the release image")).toBeInTheDocument();
    expect(screen.getByText("3.6.0 at start, now 3.6.1")).toBeInTheDocument();

    fireEvent.click(screen.getAllByRole("button", { name: "Revert to default" })[0]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("redhat-ods-applications/dashboard-operator")).toBeInTheDocument();
    expect(within(dialog).getByText(/ending alice's session/)).toBeInTheDocument();
    expect(api.calls.some((c) => c.startsWith("POST /api/dashboard/revert"))).toBe(false);
    fireEvent.click(within(dialog).getByRole("button", { name: "Revert to default" }));
    expect(await screen.findByText("Reverted")).toBeInTheDocument();
    expect(api.calls.filter((c) => c === "POST /api/dashboard/revert")).toHaveLength(1);
  });

  it("a 404 with a paused operator still offers Revert and flags the blocked Dashboard deletion (D1, A04-9)", async () => {
    stubApi({
      "/api/dashboard/state": () => jsonResponse({
        error: "failed to get dashboard state: deployment not found", errorCode: "dashboard_not_deployed",
        override: { ...session, dashboardDeleting: true, stale: false, warnings: ["A Dashboard CR is being deleted while dashboard-operator is paused."] },
      }, 404),
    });
    renderPage();
    expect(await screen.findByText("The RHOAI dashboard is not deployed")).toBeInTheDocument();
    expect(screen.getByText("Dashboard deletion is blocked until you revert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revert to default" })).toBeInTheDocument();
  });

  it.each([
    [403, "forbidden", "Access denied"],
    [503, "network", "The updater cannot reach the cluster API"],
    [502, "unauthorized", "The cluster rejected the updater's credentials"],
  ])("HTTP %i %s is an error, not 'not deployed' (A04-9)", async (status, errorCode, title) => {
    stubApi({ "/api/dashboard/state": () => jsonResponse({ error: "boom", errorCode }, status) });
    renderPage();
    expect(await screen.findByText(new RegExp(`^${title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`))).toBeInTheDocument();
    expect(screen.queryByText(/is not deployed/)).not.toBeInTheDocument();
  });

  it("overrideAlerts titles each backend warning by its flag", () => {
    const alerts = overrideAlerts({ ...session, dashboardDeleting: true, sessionRecorded: false, warnings: ["deleting text", "stale text", "external text"] });
    expect(alerts.map((a) => [a.title, a.body, a.variant])).toEqual([
      ["Dashboard deletion is blocked until you revert", "deleting text", "danger"],
      ["RHOAI was updated while dashboard-operator is paused", "stale text", "warning"],
      ["dashboard-operator was paused outside Dashboard Dev", "external text", "warning"],
    ]);
  });
});

describe("Dashboard Dev deploy confirmations (A04-4, A08-4) and flavor (A04-3)", () => {
  it("Deploy latest main asks first and says what changes", async () => {
    const api = stubApi({ "/api/dashboard/state": clean(), "POST /api/dashboard/deploy-main": { success: true, message: "Deploying main", logs: [] } });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Deploy latest main" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/is scaled to 0 so it stops resetting the dashboard images/)).toBeInTheDocument();
    expect(within(dialog).getByText(/All 2 dashboard containers are updated/)).toBeInTheDocument();
    expect(api.calls.some((c) => c.startsWith("POST"))).toBe(false);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.calls.some((c) => c.startsWith("POST /api/dashboard"))).toBe(false);
  });

  it("deploys a PR with the chosen ODH flavor and names the session it replaces", async () => {
    const api = stubApi({ "/api/dashboard/state": active(), "POST /api/dashboard/deploy-pr": { success: true, message: "Deploying PR #333", logs: [] } });
    renderPage();
    fireEvent.click(await screen.findByRole("radio", { name: /ODH build/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "Pull request number" }), { target: { value: "333" } });
    fireEvent.click(screen.getByRole("button", { name: "Deploy PR" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("pr-333")).toBeInTheDocument();
    expect(within(dialog).getByText(/This replaces alice's session/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Deploy PR #333" }));
    await screen.findByText("Deploying PR #333");
    expect(api.bodies["POST /api/dashboard/deploy-pr"]).toEqual([{ pr: 333, flavor: "odh" }]);
  });

  it("the default flavor is RHOAI and is sent explicitly", async () => {
    const api = stubApi({ "/api/dashboard/state": clean(), "POST /api/dashboard/deploy-main": { success: true, message: "ok", logs: [] } });
    renderPage();
    expect(await screen.findByRole("radio", { name: /RHOAI build/ })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Deploy latest main" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Deploy latest main" }));
    await screen.findByText("ok");
    expect(api.bodies["POST /api/dashboard/deploy-main"]).toEqual([{ flavor: "rhoai" }]);
  });

  it("a stuck rollout shows the server's reason and the container's waiting reason (A04-7)", async () => {
    stubApi({
      "/api/dashboard/state": active({
        rolloutStuck: true, allDevImagesReady: false,
        stuckReason: 'notebooks-ui/notebooks-ui: ReplicaSet "notebooks-ui-6c9f" has timed out progressing. (ImagePullBackOff)',
        devImages: [image("rhods-dashboard"), image("notebooks-ui", { ready: false, waitingReason: "ImagePullBackOff", waitingMessage: "Back-off pulling image", podName: "notebooks-ui-6c9f-x" })],
      }),
    });
    renderPage();
    expect(await screen.findByText("The dashboard rollout is stuck")).toBeInTheDocument();
    expect(screen.getByText(/has timed out progressing/)).toBeInTheDocument();
    expect(screen.getByText("ImagePullBackOff")).toBeInTheDocument();
    expect(screen.getByText("Pod: notebooks-ui-6c9f-x")).toBeInTheDocument();
  });

  it("Assist rollout asks first and targets rhods-dashboard; nothing to do is info", async () => {
    const api = stubApi({
      "/api/dashboard/state": clean({ rolloutPending: true, schedulingFailureReason: "Unschedulable", canAssistRollout: true }),
      "POST /api/assist-rollout": () => jsonResponse({ success: false, message: "Nothing to do: the pod is scheduled now.", logs: [], errorCode: "nothing_to_do" }, 422),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Assist rollout" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("redhat-ods-applications/rhods-dashboard")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Assist rollout" }));
    expect(await screen.findByText("Nothing to do")).toBeInTheDocument();
    expect(api.bodies["POST /api/assist-rollout"]).toEqual([{ namespace: "redhat-ods-applications", deployment: "rhods-dashboard" }]);
  });

  it("Assist rollout is available while the page waits for the rollout it rescues (R4b e)", async () => {
    let state = clean();
    stubApi({ "/api/dashboard/state": () => jsonResponse(state), "POST /api/dashboard/deploy-main": () => {
      state = active({ devMode: "main", rolloutPending: true, allDevImagesReady: false, schedulingFailureReason: "0/3 nodes are available: 3 Insufficient cpu.", canAssistRollout: true });
      return jsonResponse({ success: true, message: "Deploying main", logs: [] });
    } });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Deploy latest main" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Deploy latest main" }));
    expect(await screen.findByText(/Waiting for the dashboard pods to roll out/)).toBeInTheDocument();
    const assist = await screen.findByRole("button", { name: "Assist rollout" });
    expect(assist).not.toHaveAttribute("aria-disabled");
    // Deploying again still waits for this rollout.
    expect(screen.getByRole("button", { name: "Deploy latest main" })).toHaveAttribute("aria-disabled", "true");
  });

  it("stops waiting as soon as a container reports a terminal waiting reason (R4b e)", async () => {
    let state = clean();
    stubApi({ "/api/dashboard/state": () => jsonResponse(state), "POST /api/dashboard/deploy-main": () => {
      state = active({ devMode: "main", allDevImagesReady: false, rolloutStuck: false,
        devImages: [image("rhods-dashboard"), image("notebooks-ui", { ready: false, waitingReason: "ImagePullBackOff", waitingMessage: "Back-off pulling image" })] });
      return jsonResponse({ success: true, message: "Deploying main", logs: [] });
    } });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Deploy latest main" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Deploy latest main" }));
    expect(await screen.findByText("A dashboard container cannot start")).toBeInTheDocument();
    expect(screen.queryByText(/Waiting for the dashboard pods to roll out/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Deploy latest main" })).not.toHaveAttribute("aria-disabled");
  });

  it("a teammate's running operation disables deploys with the reason (N1)", async () => {
    stubApi({
      "/api/dashboard/state": clean(),
    });
    renderWithApp(<DashboardDevPage />, {
      permissions: async () => ({ canMutate: true, user: "me" }),
      operation: async () => ({ inProgress: true, operation: { id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } }),
    }, "/dashboard-dev");
    const main = await screen.findByRole("button", { name: "Deploy latest main" });
    await waitFor(() => expect(main).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(main);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/alice is running "Update to nightly"/);
  });

  it("refreshes the app-wide Dashboard Dev override after a change (N2)", async () => {
    stubApi({ "/api/dashboard/state": clean(), "POST /api/dashboard/deploy-main": { success: true, message: "Deploying main", logs: [] } });
    const dashboard = vi.fn(async () => clean());
    renderWithApp(<DashboardDevPage />, { permissions: async () => ({ canMutate: true, user: "me" }), dashboard }, "/dashboard-dev");
    fireEvent.click(await screen.findByRole("button", { name: "Deploy latest main" }));
    await waitFor(() => expect(dashboard).toHaveBeenCalledTimes(1));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Deploy latest main" }));
    await waitFor(() => expect(dashboard).toHaveBeenCalledTimes(2));
  });

  it("read-only users cannot open the deploy dialogs", async () => {
    stubApi({ "/api/dashboard/state": clean() });
    renderPage(false);
    const main = await screen.findByRole("button", { name: "Deploy latest main" });
    expect(main).toHaveAttribute("aria-disabled", "true");
    fireEvent.click(main);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
