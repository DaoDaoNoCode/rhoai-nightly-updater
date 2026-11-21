import { describe, expect, it } from "vitest";
import { act, fireEvent, screen } from "@testing-library/react";
import { DashboardDevPage } from "./pages/DashboardDevPage";
import { useDashboardOverride } from "./state/AppInfo";
import { stubApi, jsonResponse } from "./test/apiStub";
import { ADMIN_PERMISSIONS, renderWithApp, settleApps } from "./test/providers";
import type { DashboardState } from "./types";

// GET /api/dashboard/state is read by the Dashboard Dev page (its poll and
// Refresh) and, on demand, by the app (refresh() after an operation ended
// or a session changed). Whichever read STARTED later describes the
// cluster later: its answer wins, whatever order the answers arrive in.

function state(by: string): DashboardState {
  return {
    operatorAvailable: false, operatorPaused: false, isDevMode: false, allDevImagesReady: true, defaultImagesRestored: true, currentImage: "img",
    isCustomPR: false, managed: true, podStatus: "Running", podReady: true, containersReady: 1, containersTotal: 1, rolloutPending: false, canAssistRollout: false, devImages: [],
    override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false, startedBy: by },
  } as unknown as DashboardState;
}

/** The page's reads (fetch) and the app's reads (injected) answer when the test says. */
function setup() {
  const page: Array<(body: DashboardState) => void> = [];
  const app: Array<(body: DashboardState) => void> = [];
  stubApi({
    "/api/dashboard/state": () => new Promise<Response>((resolve) => { page.push((body) => resolve(jsonResponse(body))); }),
    "/api/resources/status": { minio: { deployed: false, ready: false }, mlflow: { deployed: false, ready: false }, pipelineServers: [] },
  });
  const Probe = () => {
    const d = useDashboardOverride();
    return (
      <>
        <button type="button" onClick={d.refresh}>app refresh</button>
        <p data-testid="override">{d.override?.startedBy ?? "-"}</p>
      </>
    );
  };
  const rendered = renderWithApp(<><DashboardDevPage /><Probe /></>, {
    permissions: async () => ADMIN_PERMISSIONS,
    dashboard: () => new Promise<DashboardState>((resolve) => { app.push(resolve); }),
  }, "/dashboard-dev");
  return { page, app, rendered };
}

const shown = () => screen.getByTestId("override").textContent;
const appRefresh = () => fireEvent.click(screen.getByRole("button", { name: "app refresh" }));
const pageRefresh = () => fireEvent.click(screen.getByRole("button", { name: "Refresh dashboard dev" }));

describe("the Dashboard Dev state read by the page and by the app", () => {
  // Both reads stay open until the test answers them; in between, every
  // other read answers inside act (await act(async () => {})).
  it("page read starts, then the app's: the app's answer stays when the page's arrives last", async () => {
    const { page, app } = setup();
    await act(async () => {});
    expect(page).toHaveLength(1);
    appRefresh();
    expect(app).toHaveLength(1);
    await act(async () => app[0](state("newer")));
    expect(shown()).toBe("newer");
    await act(async () => page[0](state("older")));
    await settleApps();
    expect(shown()).toBe("newer");
  });

  it("page read starts, then the app's: the app's answer wins when the page's arrives first", async () => {
    const { page, app } = setup();
    await act(async () => {});
    expect(page).toHaveLength(1);
    appRefresh();
    expect(app).toHaveLength(1);
    await act(async () => page[0](state("older")));
    await act(async () => app[0](state("newer")));
    await settleApps();
    expect(shown()).toBe("newer");
  });

  it("app read starts, then the page's: the page's answer wins when it arrives first", async () => {
    const { page, app } = setup();
    await act(async () => {});
    expect(page).toHaveLength(1);
    await act(async () => page[0](state("first")));
    expect(shown()).toBe("first");
    appRefresh();
    expect(app).toHaveLength(1);
    pageRefresh();
    await act(async () => {});
    expect(page).toHaveLength(2);
    await act(async () => page[1](state("newer")));
    await act(async () => app[0](state("older")));
    await settleApps();
    expect(shown()).toBe("newer");
  });

  it("app read starts, then the page's: the page's answer wins when it arrives last", async () => {
    const { page, app } = setup();
    await act(async () => {});
    expect(page).toHaveLength(1);
    await act(async () => page[0](state("first")));
    expect(shown()).toBe("first");
    appRefresh();
    expect(app).toHaveLength(1);
    pageRefresh();
    await act(async () => {});
    expect(page).toHaveLength(2);
    await act(async () => app[0](state("older")));
    await act(async () => page[1](state("newer")));
    await settleApps();
    expect(shown()).toBe("newer");
  });
});
