import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { busyOperationFrom, isOwnFinishedRun, matchesRun, snapshotFromServer, useOperation } from "./AppState";
import { useMutationBlocker } from "./AppInfo";
import { ApiError, streamUpdate } from "../services/api";
import type { OperationStatusResponse, ServerOperation } from "../types";
import { IDLE_OPERATION, nightlyStatus, renderWithApp } from "../test/providers";
import { deferred, jsonResponse, sseEvent, sseResponse } from "../test/utils";
import type { OperationRun } from "./operation";

const aliceUpdate: ServerOperation = {
  id: "op1", type: "update", label: "Update to nightly", user: "alice",
  target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64),
  startedAt: "2026-10-05T10:00:00Z", step: "wait_catalog_ready", stepStatus: "running", message: "CatalogSource: CONNECTING",
};

const Probe: React.FC = () => {
  const op = useOperation();
  const blocker = useMutationBlocker();
  return (
    <pre data-testid="probe">
      {JSON.stringify({
        running: op.running,
        source: op.run?.source,
        kind: op.run?.kind,
        user: op.run?.user,
        steps: op.run?.steps.map((s) => `${s.step}:${s.status}`),
        outcome: op.run?.outcome?.status,
        reason: op.run?.outcome?.status === "detached" ? op.run.outcome.reason : undefined,
        serverInProgress: op.server.inProgress,
        serverUser: op.server.operation?.user,
        interrupted: op.server.interrupted?.label,
        serverId: op.run?.serverId,
        phase: op.phase,
        result: op.state.reconcile.result,
        message: op.run?.outcome?.message,
        blocker,
      })}
    </pre>
  );
};

const StartUpdate: React.FC = () => {
  const op = useOperation();
  return <button onClick={() => op.start("update", (h) => streamUpdate("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", h.onStep, h.onDone, h.onDetach))}>start</button>;
};

const probe = () => JSON.parse(screen.getByTestId("probe").textContent || "{}");

describe("snapshotFromServer", () => {
  it("marks the steps before the reported one as done and maps the kind", () => {
    const snap = snapshotFromServer(aliceUpdate);
    expect(snap?.kind).toBe("update");
    expect(snap?.user).toBe("alice");
    expect(snap?.steps.map((s) => `${s.step}:${s.status}`)).toEqual([
      "validate_prerequisites:success", "save_snapshot:success", "apply_catalog_source:success", "wait_catalog_ready:running",
    ]);
  });

  it("keeps the pod only for an operation another updater pod runs", () => {
    expect(snapshotFromServer({ ...aliceUpdate, pod: "updater-a" })?.pod).toBeUndefined();
    expect(snapshotFromServer({ ...aliceUpdate, pod: "updater-a", remote: true })?.pod).toBe("updater-a");
  });

  it("maps reinstall targets and ignores operations without steps", () => {
    expect(snapshotFromServer({ ...aliceUpdate, type: "reinstall", target: "stable", step: undefined })?.kind).toBe("reinstall_stable");
    expect(snapshotFromServer({ ...aliceUpdate, type: "reinstall", target: "nightly quay.io/x:y" })?.kind).toBe("reinstall_nightly");
    expect(snapshotFromServer({ ...aliceUpdate, type: "deploy-dashboard-pr" })).toBeNull();
  });
});

describe("busyOperationFrom", () => {
  it("reads the operation from a 409 cluster_busy body and rejects junk", () => {
    const err = new ApiError({ status: 409, errorCode: "cluster_busy", message: "busy", details: { operation: aliceUpdate } });
    expect(busyOperationFrom(err)?.user).toBe("alice");
    expect(busyOperationFrom(new ApiError({ status: 409, errorCode: "cluster_busy", message: "busy", details: { operation: { id: 1 } } }))).toBeUndefined();
    expect(busyOperationFrom(undefined)).toBeUndefined();
  });

  it("keeps the pod of an operation another updater pod runs", () => {
    const err = new ApiError({ status: 409, errorCode: "cluster_busy", message: "busy", details: { operation: { ...aliceUpdate, remote: true, pod: "updater-a" } } });
    expect(busyOperationFrom(err)).toMatchObject({ remote: true, pod: "updater-a" });
  });
});

describe("an operation another updater pod runs (remote)", () => {
  it("follows it by polling, names the pod, and takes the result from lastCompleted", async () => {
    const remote: ServerOperation = { ...aliceUpdate, remote: true, pod: "updater-old" };
    let answer: OperationStatusResponse = { inProgress: true, operation: remote };
    renderWithApp(<><Probe /><RefreshButton /></>, { operation: async () => answer });
    await waitFor(() => expect(probe()).toMatchObject({ running: true, source: "server", serverInProgress: true, serverId: "op1" }));
    expect(probe().blocker).toMatch(/alice is running "Update to nightly" on updater pod updater-old/);
    expect(probe().steps).toContain("wait_catalog_ready:running");

    answer = { inProgress: false, operation: null, lastCompleted: { id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: aliceUpdate.startedAt, finishedAt: "2026-10-05T10:09:00Z", success: true, message: "Update complete" } };
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe()).toMatchObject({ outcome: "succeeded", serverInProgress: false }));
  });
});

describe("isOwnFinishedRun (R4b b: match by backend id, not a time window)", () => {
  const started = Date.parse(aliceUpdate.startedAt);
  const base: OperationRun = { id: 1, kind: "update", source: "stream", startedAt: started, steps: [], endedAt: started + 120_000, outcome: { status: "succeeded", message: "" }, serverId: "op1" };
  it("hides only the operation with this run's backend id", () => {
    expect(isOwnFinishedRun(base, aliceUpdate)).toBe(true);
    // A teammate's update that started a second after this run ended is not this run.
    expect(isOwnFinishedRun(base, { ...aliceUpdate, id: "op2", startedAt: new Date(started + 121_000).toISOString() })).toBe(false);
  });
  it("without a backend id, needs the same kind and a start within the tolerance", () => {
    const unbound: OperationRun = { ...base, serverId: undefined };
    expect(isOwnFinishedRun(unbound, aliceUpdate)).toBe(true);
    expect(isOwnFinishedRun(unbound, { ...aliceUpdate, type: "refresh" })).toBe(false);
    expect(isOwnFinishedRun(unbound, { ...aliceUpdate, startedAt: new Date(started + 121_000).toISOString() })).toBe(false);
  });
  it("never hides an operation behind a rejected request or a detached run", () => {
    const rejected: OperationRun = { ...base, outcome: { status: "failed", message: "busy", rejected: true, errorCode: "cluster_busy" } };
    expect(isOwnFinishedRun(rejected, aliceUpdate)).toBe(false);
    const detached: OperationRun = { ...base, outcome: { status: "detached", reason: "connection_lost", message: "x" } };
    expect(isOwnFinishedRun(detached, aliceUpdate)).toBe(false);
  });
  it("matchesRun prefers the id", () => {
    expect(matchesRun(base, { ...aliceUpdate, id: "zzz" })).toBe(false);
    expect(matchesRun(null, aliceUpdate)).toBe(false);
  });
});

describe("GET /api/operation drives the operation store (A07-3, A06-3)", () => {
  it("shows a teammate's running update after a reload, disables changes, and hands over when it ends", async () => {
    let answer: OperationStatusResponse = { inProgress: true, operation: aliceUpdate };
    const fetchOperation = vi.fn(async () => answer);
    renderWithApp(<><Probe /><RefreshButton /></>, { operation: fetchOperation });

    await waitFor(() => expect(probe()).toMatchObject({ running: true, source: "server", kind: "update", user: "alice", serverInProgress: true }));
    expect(probe().steps).toContain("wait_catalog_ready:running");
    expect(probe().blocker).toMatch(/alice is running "Update to nightly"/);

    answer = IDLE_OPERATION;
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe()).toMatchObject({ running: false, outcome: "detached", reason: "server_lost", serverInProgress: false }));
  });

  it("reports an operation the previous pod never finished", async () => {
    renderWithApp(<Probe />, {
      operation: async () => ({ inProgress: false, operation: null, interrupted: { type: "reinstall", label: "Reinstall operator", user: "alice", startedAt: "2026-10-05T09:00:00Z" } }),
    });
    await waitFor(() => expect(probe().interrupted).toBe("Reinstall operator"));
    expect(probe().running).toBe(false);
  });

  it("does not create a run for operations without steps, but still blocks changes", async () => {
    renderWithApp(<Probe />, {
      operation: async () => ({ inProgress: true, operation: { ...aliceUpdate, type: "deploy-dashboard-pr", label: "Deploy dashboard PR", step: undefined } }),
    });
    await waitFor(() => expect(probe().serverInProgress).toBe(true));
    expect(probe().running).toBe(false);
    expect(probe().blocker).toMatch(/alice is running "Deploy dashboard PR"/);
  });

  it("takes the running operation from a 409 cluster_busy body at once (no wait for the next poll)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "alice is running \"Update to nightly\". Wait for it to finish.", errorCode: "cluster_busy", operation: aliceUpdate }, 409)));
    renderWithApp(<><Probe /><StartUpdate /></>);
    await waitFor(() => expect(probe().serverInProgress).toBe(false));
    fireEvent.click(screen.getByText("start"));
    await waitFor(() => expect(probe()).toMatchObject({ serverInProgress: true, serverUser: "alice", source: "server", running: true }));
  });

  it("fails closed while permissions are unknown", async () => {
    renderWithApp(<Probe />, { permissions: async () => { throw new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot verify mutation permissions" }); } });
    await waitFor(() => expect(probe().blocker).toMatch(/permissions could not be checked/));
  });
});

describe("a detached stream is followed on the server (R4b a, b)", () => {
  it("keeps the lock and the banner while the backend runs it, and never reports success from the old CSV", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    const mine: ServerOperation = { ...aliceUpdate, id: "mine", user: "me", startedAt: new Date().toISOString() };
    let answer: OperationStatusResponse = IDLE_OPERATION;
    const fetchOperation = vi.fn(async () => answer);
    // The old operator stays Succeeded throughout.
    renderWithApp(<><Probe /><StartUpdate /><RefreshButton /></>, { operation: fetchOperation, status: async () => nightlyStatus() });
    await waitFor(() => expect(fetchOperation).toHaveBeenCalled());

    fireEvent.click(screen.getByText("start"));
    answer = { inProgress: true, operation: mine };
    sse.push(sseEvent("validate_prerequisites", "running"));
    // The first event binds the run to its backend id.
    await waitFor(() => expect(probe()).toMatchObject({ running: true, source: "stream", serverId: "mine" }));

    sse.fail();
    // The stream is gone but the server still runs it: followed as a server run, still locked.
    await waitFor(() => expect(probe()).toMatchObject({ running: true, source: "server", serverInProgress: true, serverId: "mine" }));
    expect(probe().blocker).toMatch(/You are running "Update to nightly"/);
    expect(probe().phase).toBe("streaming");

    answer = { inProgress: false, operation: null, lastCompleted: { id: "mine", type: "update", label: "Update to nightly", user: "me", startedAt: mine.startedAt, finishedAt: new Date().toISOString(), success: true, message: "Update complete" } };
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe()).toMatchObject({ outcome: "succeeded", phase: "complete", result: "succeeded", serverInProgress: false }));
  });

  it("does not hide a teammate's operation that starts right after this tab's run (other id)", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    const mine: ServerOperation = { ...aliceUpdate, id: "mine", user: "me", startedAt: new Date().toISOString() };
    let answer: OperationStatusResponse = IDLE_OPERATION;
    const fetchOperation = vi.fn(async () => answer);
    renderWithApp(<><Probe /><StartUpdate /><RefreshButton /></>, { operation: fetchOperation });
    await waitFor(() => expect(fetchOperation).toHaveBeenCalled());
    fireEvent.click(screen.getByText("start"));
    answer = { inProgress: true, operation: mine };
    sse.push(sseEvent("validate_prerequisites", "running"));
    await waitFor(() => expect(probe().serverId).toBe("mine"));

    // This tab's own operation, still holding the lock right after its last event: hidden.
    sse.push(sseEvent("operation_complete", "success", "Update applied"));
    sse.close();
    await waitFor(() => expect(probe().outcome).toBe("succeeded"));
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(fetchOperation.mock.calls.length).toBeGreaterThan(2));
    expect(probe().serverInProgress).toBe(false);

    // Alice's update a second later is shown and blocks changes at once.
    answer = { inProgress: true, operation: { ...aliceUpdate, startedAt: new Date().toISOString() } };
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe()).toMatchObject({ serverInProgress: true, serverUser: "alice", source: "server" }));
    expect(probe().blocker).toMatch(/alice is running/);
  });
});

describe("the outcome of an operation followed from the server (N4)", () => {
  const followAlice = async (last: OperationStatusResponse["lastCompleted"]) => {
    let answer: OperationStatusResponse = { inProgress: true, operation: aliceUpdate };
    renderWithApp(<><Probe /><RefreshButton /></>, { operation: async () => answer });
    await waitFor(() => expect(probe()).toMatchObject({ running: true, source: "server" }));
    answer = { inProgress: false, operation: null, lastCompleted: last };
    fireEvent.click(screen.getByText("poll"));
  };

  it("shows a failed update as failed, with the server's message", async () => {
    await followAlice({ id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: aliceUpdate.startedAt, finishedAt: "2026-10-05T10:09:00Z", success: false, message: "The new CSV failed; the previous operator was restored." });
    await waitFor(() => expect(probe()).toMatchObject({ outcome: "failed", phase: "idle", message: "The new CSV failed; the previous operator was restored." }));
  });

  it("shows a successful update as succeeded", async () => {
    await followAlice({ id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: aliceUpdate.startedAt, success: true, message: "done" });
    await waitFor(() => expect(probe()).toMatchObject({ outcome: "succeeded", result: "succeeded" }));
  });

  it("claims nothing when the backend sends no result, or the result of another operation", async () => {
    await followAlice({ id: "someone-else", type: "update", label: "Update", user: "bob", startedAt: aliceUpdate.startedAt, success: true });
    await waitFor(() => expect(probe()).toMatchObject({ outcome: "detached", reason: "server_lost" }));
    await waitFor(() => expect(probe().phase).toBe("complete"));
    expect(probe().result).toBe("unknown");
  });
});

describe("GET /api/operation answers are applied in order (N6)", () => {
  it("drops an idle answer that lands after a newer busy one", async () => {
    const first = deferred<OperationStatusResponse>();
    const calls: Array<Promise<OperationStatusResponse>> = [first.promise, Promise.resolve({ inProgress: true, operation: aliceUpdate })];
    const fetchOperation = vi.fn(() => calls.shift() ?? Promise.resolve({ inProgress: true, operation: aliceUpdate }));
    renderWithApp(<><Probe /><RefreshButton /></>, { operation: fetchOperation });
    await waitFor(() => expect(fetchOperation).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe().serverInProgress).toBe(true));
    await act(async () => { first.resolve(IDLE_OPERATION); await first.promise; });
    expect(probe()).toMatchObject({ serverInProgress: true, serverUser: "alice", running: true });
  });
});

describe("a reinstall whose target arrives after the first poll (N5)", () => {
  it("switches the run to the stable reinstall steps", async () => {
    let answer: OperationStatusResponse = { inProgress: true, operation: { ...aliceUpdate, id: "r1", type: "reinstall", label: "Reinstall", target: undefined, step: "save_snapshot" } };
    renderWithApp(<><Probe /><RefreshButton /></>, { operation: async () => answer });
    await waitFor(() => expect(probe()).toMatchObject({ kind: "reinstall_nightly", running: true }));
    answer = { inProgress: true, operation: { ...aliceUpdate, id: "r1", type: "reinstall", label: "Reinstall", target: "stable", step: "create_subscription" } };
    fireEvent.click(screen.getByText("poll"));
    await waitFor(() => expect(probe().kind).toBe("reinstall_stable"));
    expect(probe().steps).toContain("create_subscription:running");
  });
});

const RefreshButton: React.FC = () => {
  const op = useOperation();
  return <button onClick={op.refreshServerOperation}>poll</button>;
};
