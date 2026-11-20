import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useMaaSStatus } from "./components/environment/MaaSTab";
import { usePlaygroundStatus } from "./components/environment/usePlaygroundStatus";
import { useSampleStatus } from "./components/environment/useSampleStatus";
import { AppStateProvider } from "./state/AppState";
import { LiveAnnouncerProvider } from "./state/LiveAnnouncer";
import { useDashboardOverride } from "./state/AppInfo";
import { TroubleshootingPage } from "./pages/TroubleshootingPage";
import { DashboardDevPage } from "./pages/DashboardDevPage";
import { getComponents } from "./services/api";
import { stubApi, jsonResponse } from "./test/apiStub";
import { ADMIN_PERMISSIONS, nightlyStatus, renderPage, renderWithApp, settleApps } from "./test/providers";
import { BACKGROUND_POLL_MS, COMPONENTS_POLL_MS, DASHBOARD_OVERRIDE_POLL_MS, OPERATION_POLL_IDLE_MS } from "./constants";
import type { DashboardState, DiagnosticResult, OperationStatusResponse, Problem, StatusResponse } from "./types";

// The polling gaps of the frontend audit (qa-frontend.md A4-A7): each test
// shows one. Answers are controlled promises, resolved in the order a slow
// network could deliver them.

/** A fetch whose answers for `path` the test hands out, in call order. */
function heldAnswers(path: string, extra: Record<string, unknown> = {}) {
  const held: Array<{ resolve: (body: unknown) => void; signal: AbortSignal | null | undefined }> = [];
  const api = stubApi({
    ...extra,
    [path]: (_url: string, init: RequestInit | undefined) => new Promise<Response>((resolve) => {
      held.push({ resolve: (body) => resolve(jsonResponse(body)), signal: init?.signal });
    }),
  });
  return { api, held };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("(c) an older answer never overwrites a newer one", () => {
  it("MaaS: a reload's answer, then the earlier read's", async () => {
    const { held } = heldAnswers("/api/resources/maas");
    let reload: () => void = () => {};
    const Probe = () => {
      const m = useMaaSStatus(0, false);
      reload = m.reload;
      return <p data-testid="maas">{m.data?.subscription ?? "none"}</p>;
    };
    render(<Probe />);
    await waitFor(() => expect(held).toHaveLength(1));
    act(() => { reload(); });
    await waitFor(() => expect(held).toHaveLength(2));
    await act(async () => held[1].resolve({ platform: { deployed: true, ready: true }, parts: [], subscription: "newer" }));
    await waitFor(() => expect(screen.getByTestId("maas")).toHaveTextContent("newer"));
    await act(async () => held[0].resolve({ platform: { deployed: true, ready: true }, parts: [], subscription: "older" }));
    await act(async () => {});
    expect(screen.getByTestId("maas")).toHaveTextContent("newer");
  });

  it("Playground: two reads for the same project", async () => {
    const { held } = heldAnswers("/api/resources/playground");
    const Probe = ({ refreshKey }: { refreshKey: number }) => {
      const pg = usePlaygroundStatus("dev", refreshKey, false);
      return <p data-testid="pg">{pg.data?.operatorMessage ?? "none"}</p>;
    };
    const { rerender } = render(<Probe refreshKey={0} />);
    await waitFor(() => expect(held).toHaveLength(1));
    rerender(<Probe refreshKey={1} />);
    await waitFor(() => expect(held).toHaveLength(2));
    await act(async () => held[1].resolve({ project: "dev", items: [], operatorMessage: "newer" }));
    await waitFor(() => expect(screen.getByTestId("pg")).toHaveTextContent("newer"));
    await act(async () => held[0].resolve({ project: "dev", items: [], operatorMessage: "older" }));
    await act(async () => {});
    expect(screen.getByTestId("pg")).toHaveTextContent("newer");
  });

  it("Samples: two reads for the same project", async () => {
    const answers: Array<(v: string) => void> = [];
    const load = vi.fn(() => new Promise<string>((resolve) => { answers.push(resolve); }));
    const Probe = ({ refreshKey }: { refreshKey: number }) => {
      const s = useSampleStatus(load, "dev", refreshKey);
      return <p data-testid="sample">{s.data ?? "none"}</p>;
    };
    const { rerender } = render(<Probe refreshKey={0} />);
    await waitFor(() => expect(answers).toHaveLength(1));
    rerender(<Probe refreshKey={1} />);
    await waitFor(() => expect(answers).toHaveLength(2));
    await act(async () => answers[1]("newer"));
    await waitFor(() => expect(screen.getByTestId("sample")).toHaveTextContent("newer"));
    await act(async () => answers[0]("older"));
    await act(async () => {});
    expect(screen.getByTestId("sample")).toHaveTextContent("newer");
  });

  it("Diagnostics: the rescan after a fix answers before the Refresh that was already running", async () => {
    const fixable: Problem = {
      id: "webhook-stale", severity: "warning", title: "1 webhook configuration points to a missing Service",
      description: "Leftover webhook.", fix: "Delete it.", autoFixable: true, autoFixAction: "delete-stale-webhooks",
      confirmMessage: "Delete the webhook configurations listed below.", affectedObjects: ["ValidatingWebhookConfiguration x"],
    };
    const scan = (problems: Problem[]): DiagnosticResult => ({ problems, checks: [{ name: "Catalog health", status: "pass", detail: "ok" }] });
    const { held } = heldAnswers("/api/diagnostics", { "POST /api/diagnostics/fix": { success: true, message: "Deleted.", logs: [] } });
    renderPage(<TroubleshootingPage />, "/diagnostics");
    await waitFor(() => expect(held).toHaveLength(1));
    await act(async () => held[0].resolve(scan([fixable])));
    await screen.findByText(fixable.title);
    // Refresh: a scan that takes long.
    fireEvent.click(screen.getByRole("button", { name: "Re-scan diagnostics" }));
    await waitFor(() => expect(held).toHaveLength(2));
    // Meanwhile the problem is fixed, and the page scans again.
    const card = screen.getByText(fixable.title).closest(".pf-v6-c-card") as HTMLElement;
    fireEvent.click(within(card).getByRole("button", { name: /Details/ }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Fix" })).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Fix" }));
    await waitFor(() => expect(held).toHaveLength(3));
    // The newer scan (after the fix) answers first: nothing left.
    await act(async () => held[2].resolve(scan([])));
    await waitFor(() => expect(screen.queryByText(fixable.title)).not.toBeInTheDocument());
    // Then the older Refresh answers, from before the fix.
    await act(async () => held[1].resolve(scan([fixable])));
    await act(async () => {});
    expect(screen.queryByText(fixable.title)).not.toBeInTheDocument();
    await settleApps();
  });
});

describe("(d) a polled read in flight is cancelled when its page goes away", () => {
  it("the cluster status read", async () => {
    const signals: Array<AbortSignal | undefined> = [];
    const fetchStatus = vi.fn((signal?: AbortSignal) => {
      signals.push(signal);
      return new Promise<StatusResponse>(() => {});
    });
    const { unmount } = render(
      <LiveAnnouncerProvider>
        <MemoryRouter>
          <AppStateProvider fetchStatus={fetchStatus} fetchOperation={async () => ({ inProgress: false, operation: null })}>
            <p>page</p>
          </AppStateProvider>
        </MemoryRouter>
      </LiveAnnouncerProvider>,
    );
    await waitFor(() => expect(fetchStatus).toHaveBeenCalled());
    unmount();
    expect(signals[0]?.aborted).toBe(true);
  });

  it("the MaaS read", async () => {
    const { held } = heldAnswers("/api/resources/maas");
    const Probe = () => {
      useMaaSStatus(0, true);
      return null;
    };
    const { unmount } = render(<Probe />);
    await waitFor(() => expect(held).toHaveLength(1));
    unmount();
    expect(held[0].signal?.aborted).toBe(true);
  });
});

describe("(a) polls stop once the session expired", () => {
  it("no more operation, status, permission or Dashboard Dev reads after a 401 session_expired", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const counts = { status: 0, operation: 0, permissions: 0, dashboard: 0 };
    const app = renderWithApp(<p>page</p>, {
      status: async () => { counts.status++; return nightlyStatus(); },
      operation: async () => { counts.operation++; return { inProgress: false, operation: null } as OperationStatusResponse; },
      permissions: async () => { counts.permissions++; return ADMIN_PERMISSIONS; },
      dashboard: async () => { counts.dashboard++; return { override: { active: false } } as DashboardState; },
    });
    await app.settled();
    // Some page's read finds the session gone.
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Your OpenShift session has expired. Log in again.", errorCode: "session_expired" }, 401)));
    await act(async () => { await getComponents().catch(() => undefined); });
    const at = { ...counts };
    for (let i = 0; i < 20; i++) await act(() => vi.advanceTimersByTimeAsync(OPERATION_POLL_IDLE_MS));
    // 300 s: 20 operation polls, 5 status polls, 2 Dashboard Dev polls before.
    expect(counts).toEqual(at);
    await app.settled();
  });

  it("a page's own poll stops too", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    let maasReads = 0;
    let expired = false;
    const gone = () => jsonResponse({ error: "expired", errorCode: "session_expired" }, 401);
    stubApi({
      "/api/resources/maas": () => {
        maasReads++;
        return expired ? gone() : jsonResponse({ platform: { deployed: true, ready: false }, parts: [{ name: "x", state: "waiting" }], subscription: "" });
      },
      "/api/components": () => gone(),
    });
    const Probe = () => {
      useMaaSStatus(0, false);
      return null;
    };
    const app = renderWithApp(<Probe />);
    await app.settled();
    for (let i = 0; i < 3; i++) await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(maasReads).toBeGreaterThan(1);
    expired = true;
    await act(async () => { await getComponents().catch(() => undefined); });
    const at = maasReads;
    for (let i = 0; i < 6; i++) await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(maasReads).toBe(at);
    await app.settled();
  });
});

describe("(b) /api/dashboard/state is read once per poll, not twice", () => {
  it("while the Dashboard Dev page polls it, the app-wide reader does not, and gets the page's answer", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const state = { operatorAvailable: false, operatorPaused: false, isDevMode: false, allDevImagesReady: true, defaultImagesRestored: true, currentImage: "img", isCustomPR: false, managed: true, podStatus: "Running", podReady: true, containersReady: 1, containersTotal: 1, rolloutPending: false, canAssistRollout: false, devImages: [], override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false } } as unknown as DashboardState;
    let pageReads = 0;
    stubApi({
      "/api/dashboard/state": () => { pageReads++; return jsonResponse(state); },
      "/api/resources/status": { minio: { deployed: false, ready: false }, mlflow: { deployed: false, ready: false }, pipelineServers: [] },
    });
    let appReads = 0;
    const Probe = () => <p data-testid="override">{String(useDashboardOverride().loaded)}</p>;
    const app = renderWithApp(<><DashboardDevPage /><Probe /></>, {
      dashboard: async () => { appReads++; return state; },
    }, "/dashboard-dev");
    await app.settled();
    const before = { app: appReads, page: pageReads };
    for (let i = 0; i < 8; i++) await act(() => vi.advanceTimersByTimeAsync(COMPONENTS_POLL_MS));
    await app.settled();
    // 240 s: the page reads every 30 s; the app-wide reader (every 120 s) would read twice more.
    expect(pageReads - before.page).toBe(8);
    expect(appReads).toBe(0);
    expect(screen.getByTestId("override")).toHaveTextContent("true");
    expect(DASHBOARD_OVERRIDE_POLL_MS).toBe(4 * COMPONENTS_POLL_MS);
    expect(BACKGROUND_POLL_MS).toBe(2 * COMPONENTS_POLL_MS);
  });
});
