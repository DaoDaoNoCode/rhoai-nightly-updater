import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { AppStateProvider, useClusterStatus, useOperation, type ServerOperationSnapshot } from "./AppState";
import { LiveAnnouncerProvider } from "./LiveAnnouncer";
import { streamUpdate } from "../services/api";
import type { OperationStatusResponse, StatusResponse } from "../types";
import { deferred, jsonResponse, sseEvent, sseResponse } from "../test/utils";

function statusWith(phase: string): StatusResponse {
  return {
    cluster: { server: "s", version: "4.19", user: "me" },
    subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "fast", state: "AtLatestKnown" },
    csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase },
    catalogSource: { exists: true, name: "rhoai-catalog-dev", image: "img", state: "READY" },
    pullSecret: { exists: true, valid: true },
    imageMirror: { exists: true, name: "m", source: "registry.redhat.io/rhoai" },
    stableSource: "redhat-operators",
    stableChannel: "stable",
    dscExists: true,
  };
}

const StartButton: React.FC = () => {
  const op = useOperation();
  return (
    <button onClick={() => op.start("update", (h) => streamUpdate("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", h.onStep, h.onDone, h.onDetach))}>
      start
    </button>
  );
};

const Probe: React.FC = () => {
  const op = useOperation();
  const { status } = useClusterStatus();
  const outcome = op.run?.outcome;
  return (
    <pre data-testid="probe">
      {JSON.stringify({
        running: op.running,
        phase: op.phase,
        steps: op.run?.steps.length ?? 0,
        outcome: outcome?.status,
        reason: outcome?.status === "detached" ? outcome.reason : undefined,
        errorCode: outcome?.status === "failed" ? outcome.errorCode : undefined,
        rejected: outcome?.status === "failed" ? outcome.rejected : undefined,
        csv: status?.csv.phase,
      })}
    </pre>
  );
};

const NavButton: React.FC = () => {
  const navigate = useNavigate();
  return <button onClick={() => navigate("/components")}>leave</button>;
};

const SyncButton: React.FC<{ snapshot: ServerOperationSnapshot | null }> = ({ snapshot }) => {
  const op = useOperation();
  return <button onClick={() => op.syncServerOperation(snapshot)}>sync</button>;
};

/** GET /api/operation answering "nothing running" (these tests stub fetch for the stream only). */
const idleOperation = async () => ({ inProgress: false, operation: null });

function probe() {
  return JSON.parse(screen.getByTestId("probe").textContent || "{}");
}

function renderApp(fetchStatus: () => Promise<StatusResponse>, extra?: React.ReactNode) {
  return render(
    <LiveAnnouncerProvider>
      <MemoryRouter>
        <AppStateProvider fetchStatus={fetchStatus} fetchOperation={idleOperation}>
          <Probe />
          <NavButton />
          {extra}
          <Routes>
            <Route path="/" element={<StartButton />} />
            <Route path="/components" element={<div>components page</div>} />
          </Routes>
        </AppStateProvider>
      </MemoryRouter>
    </LiveAnnouncerProvider>,
  );
}

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

beforeEach(() => {
  vi.useFakeTimers();
  sessionStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
});

function stubStream() {
  const sse = sseResponse();
  const signals: AbortSignal[] = [];
  vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
    if (init.signal) {
      signals.push(init.signal);
      init.signal.addEventListener("abort", () => sse.fail());
    }
    return sse.response;
  }));
  return { sse, signals };
}

describe("AppStateProvider operation lifecycle", () => {
  it("keeps the stream open when the user navigates away and completes once (A06-1)", async () => {
    const phases = ["Succeeded", "Installing", "Succeeded"];
    const fetchStatus = vi.fn(async () => statusWith(phases.shift() ?? "Succeeded"));
    const { sse, signals } = stubStream();
    renderApp(fetchStatus);
    await tick();
    expect(fetchStatus).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByText("start"));
    sse.push(sseEvent("validate_prerequisites", "running"));
    await tick();
    expect(probe()).toMatchObject({ running: true, phase: "streaming", steps: 1 });

    fireEvent.click(screen.getByText("leave"));
    expect(screen.getByText("components page")).toBeInTheDocument();
    expect(signals[0].aborted).toBe(false);

    // The background poll is paused while the stream reports progress.
    await tick(60_000);
    expect(fetchStatus).toHaveBeenCalledTimes(1);

    sse.push(sseEvent("verify_installplan", "success"));
    sse.push(sseEvent("operation_complete", "success", "Update initiated"));
    sse.close();
    await tick();
    await tick();
    expect(probe()).toMatchObject({ running: false, phase: "reconciling", outcome: "succeeded" });
    // Exactly one status refresh for the completed run.
    expect(fetchStatus).toHaveBeenCalledTimes(2);
    // ...then the reconcile poll at the fast interval, which sees Succeeded.
    await tick(5_000);
    expect(fetchStatus).toHaveBeenCalledTimes(3);
    expect(probe()).toMatchObject({ phase: "complete" });
    expect(screen.getByTestId("live-region-polite")).toHaveTextContent(/reconciliation finished/i);
  });

  it("falls back to status polling when the stream drops, instead of staying 'streaming'", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Installing"));
    const { sse } = stubStream();
    renderApp(fetchStatus);
    await tick();
    fireEvent.click(screen.getByText("start"));
    sse.push(sseEvent("delete_csv", "running"));
    await tick();
    sse.fail();
    await tick();
    await tick();
    expect(probe()).toMatchObject({ running: false, phase: "reconciling", outcome: "detached", reason: "connection_lost" });
    const calls = fetchStatus.mock.calls.length;
    await tick(5_000);
    expect(fetchStatus.mock.calls.length).toBeGreaterThan(calls);
    expect(screen.getByTestId("live-region-polite")).toHaveTextContent(/continues on the server/);
  });

  it("reports a 409 cluster_busy rejection as a failed, rejected run without reconcile tracking", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Succeeded"));
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Another cluster operation is in progress. Please wait.", errorCode: "cluster_busy" }, 409)));
    renderApp(fetchStatus);
    await tick();
    fireEvent.click(screen.getByText("start"));
    await tick();
    await tick();
    expect(probe()).toMatchObject({ running: false, phase: "idle", outcome: "failed", errorCode: "cluster_busy", rejected: true });
    expect(screen.getByTestId("live-region-assertive")).toHaveTextContent("Another cluster operation is in progress");
  });

  it("ignores a second start while a run is in progress", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Succeeded"));
    stubStream();
    renderApp(fetchStatus);
    await tick();
    fireEvent.click(screen.getByText("start"));
    fireEvent.click(screen.getByText("start"));
    await tick();
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("aborts the stream when the app unmounts", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Succeeded"));
    const { signals } = stubStream();
    const view = renderApp(fetchStatus);
    await tick();
    fireEvent.click(screen.getByText("start"));
    await tick();
    view.unmount();
    expect(signals[0].aborted).toBe(true);
  });

  it("tracks an operation reported by the server and hands over to reconcile tracking when it disappears", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Installing"));
    const snapshot: ServerOperationSnapshot = {
      id: "op-1", kind: "refresh", startedAt: Date.now(), state: "running",
      steps: [{ step: "delete_csv", status: "running", message: "", elapsedMs: 0 }],
    };
    const view = renderApp(fetchStatus, <SyncButton snapshot={snapshot} />);
    await tick();
    fireEvent.click(screen.getByText("sync"));
    await tick();
    expect(probe()).toMatchObject({ running: true, steps: 1 });
    view.unmount();

    // A fresh tree simulates a later poll that no longer sees the operation.
    const fetchStatus2 = vi.fn(async () => statusWith("Installing"));
    renderApp(fetchStatus2, <><SyncButton snapshot={snapshot} /><Clear /></>);
    await tick();
    fireEvent.click(screen.getByText("sync"));
    fireEvent.click(screen.getByText("clear"));
    await tick();
    expect(probe()).toMatchObject({ running: false, phase: "reconciling", outcome: "detached", reason: "server_lost" });
  });
});

describe("reconcile tracking restored after a reload (R4b a)", () => {
  it("waits for GET /api/operation before a Succeeded CSV finishes it, and keeps the outcome unknown", async () => {
    sessionStorage.setItem("rhoai-reconciling", "true");
    sessionStorage.setItem("rhoai-reconcile-start", String(Date.now() - 1_000));
    sessionStorage.setItem("rhoai-reconcile-kind", "update");
    const op = deferred<OperationStatusResponse>();
    const fetchStatus = vi.fn(async () => statusWith("Succeeded"));
    const ResultProbe: React.FC = () => <span data-testid="result">{useOperation().state.reconcile.result}</span>;
    render(
      <LiveAnnouncerProvider>
        <MemoryRouter>
          <AppStateProvider fetchStatus={fetchStatus} fetchOperation={() => op.promise}>
            <Probe />
            <ResultProbe />
          </AppStateProvider>
        </MemoryRouter>
      </LiveAnnouncerProvider>,
    );
    await tick();
    await tick(5_000);
    expect(fetchStatus.mock.calls.length).toBeGreaterThanOrEqual(2);
    // The old operator's Succeeded does not end tracking while the server may still run the update.
    expect(probe()).toMatchObject({ phase: "reconciling" });

    op.resolve({ inProgress: false, operation: null });
    await tick();
    await tick(5_000);
    expect(probe()).toMatchObject({ phase: "complete" });
    expect(screen.getByTestId("result")).toHaveTextContent("unknown");
    expect(screen.getByTestId("live-region-polite")).toHaveTextContent(/outcome is unknown/);
  });
});

describe("background status polls (N9)", () => {
  it("do not toggle loading once a status is shown", async () => {
    const fetchStatus = vi.fn(async () => statusWith("Succeeded"));
    const renders: boolean[] = [];
    const LoadingProbe: React.FC = () => {
      renders.push(useClusterStatus().loading);
      return null;
    };
    render(
      <LiveAnnouncerProvider>
        <MemoryRouter>
          <AppStateProvider fetchStatus={fetchStatus} fetchOperation={idleOperation}>
            <LoadingProbe />
          </AppStateProvider>
        </MemoryRouter>
      </LiveAnnouncerProvider>,
    );
    await tick();
    expect(renders[renders.length - 1]).toBe(false);
    renders.length = 0;
    await tick(60_000);
    expect(fetchStatus).toHaveBeenCalledTimes(2);
    expect(renders).not.toContain(true);
  });
});

const Clear: React.FC = () => {
  const op = useOperation();
  return <button onClick={() => op.syncServerOperation(null)}>clear</button>;
};
