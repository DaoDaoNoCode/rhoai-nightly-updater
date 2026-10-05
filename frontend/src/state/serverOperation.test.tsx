import React from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { busyOperationFrom, isOwnRecentRun, snapshotFromServer, useOperation } from "./AppState";
import { useMutationBlocker } from "./AppInfo";
import { ApiError, streamUpdate } from "../services/api";
import type { OperationStatusResponse, ServerOperation } from "../types";
import { IDLE_OPERATION, renderWithApp } from "../test/providers";
import { jsonResponse } from "../test/utils";
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
});

describe("isOwnRecentRun", () => {
  const base: OperationRun = { id: 1, kind: "update", source: "stream", startedAt: 0, steps: [], endedAt: 1000, outcome: { status: "succeeded", message: "" } };
  it("treats a just-finished run of the same kind as this tab's own", () => {
    expect(isOwnRecentRun(base, "update", 5000)).toBe(true);
    expect(isOwnRecentRun(base, "refresh", 5000)).toBe(false);
    expect(isOwnRecentRun(base, "update", 60_000)).toBe(false);
  });
  it("never treats a rejected request as the running operation", () => {
    const rejected: OperationRun = { ...base, outcome: { status: "failed", message: "busy", rejected: true, errorCode: "cluster_busy" } };
    expect(isOwnRecentRun(rejected, "update", 5000)).toBe(false);
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

const RefreshButton: React.FC = () => {
  const op = useOperation();
  return <button onClick={op.refreshServerOperation}>poll</button>;
};
