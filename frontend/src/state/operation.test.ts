import { describe, expect, it } from "vitest";
import { initialOperationState, isRunning, operationPhase, operationReducer, type OperationState } from "./operation";
import type { UpdateStep } from "../types";

const step = (name: string, status: UpdateStep["status"]): UpdateStep => ({ step: name, status, message: "", elapsedMs: 0 });

function started(): OperationState {
  return operationReducer(initialOperationState(), { type: "start", id: 1, kind: "update", source: "stream", now: 100 });
}

describe("operationReducer", () => {
  it("merges step events by step name", () => {
    let s = started();
    s = operationReducer(s, { type: "step", id: 1, step: step("a", "running") });
    s = operationReducer(s, { type: "step", id: 1, step: step("b", "running") });
    s = operationReducer(s, { type: "step", id: 1, step: step("a", "success") });
    expect(s.run?.steps.map((x) => `${x.step}:${x.status}`)).toEqual(["a:success", "b:running"]);
  });

  it("ends a run once and ignores events for other or ended runs", () => {
    let s = started();
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "detached", reason: "aborted", message: "x" }, now: 200 });
    const afterFirstEnd = s;
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "succeeded", message: "late" }, now: 300 });
    s = operationReducer(s, { type: "step", id: 1, step: step("late", "running") });
    s = operationReducer(s, { type: "end", id: 2, outcome: { status: "succeeded", message: "other" }, now: 300 });
    expect(s).toBe(afterFirstEnd);
    expect(s.run?.outcome?.status).toBe("detached");
  });

  it("hands a succeeded or detached run over to reconcile tracking, but not a failed one", () => {
    const ok = operationReducer(started(), { type: "end", id: 1, outcome: { status: "succeeded", message: "ok" }, now: 500 });
    expect(ok.reconcile).toMatchObject({ active: true, startTime: 500, kind: "update" });
    const detached = operationReducer(started(), { type: "end", id: 1, outcome: { status: "detached", reason: "connection_lost", message: "x" }, now: 500 });
    expect(detached.reconcile.active).toBe(true);
    const failed = operationReducer(started(), { type: "end", id: 1, outcome: { status: "failed", message: "busy", errorCode: "cluster_busy", rejected: true }, now: 500 });
    expect(failed.reconcile.active).toBe(false);
    expect(operationPhase(failed)).toBe("idle");
  });

  it("does not start a second run while one is running", () => {
    const s = started();
    expect(operationReducer(s, { type: "start", id: 2, kind: "refresh", source: "stream", now: 1 })).toBe(s);
    expect(isRunning(s.run)).toBe(true);
  });

  it("finishes reconciliation on Succeeded or Failed and records a timeout otherwise", () => {
    let s = operationReducer(started(), { type: "end", id: 1, outcome: { status: "succeeded", message: "ok" }, now: 500 });
    expect(operationReducer(s, { type: "statusPolled", csvPhase: "Installing", requestedAt: 600 }).reconcile).toMatchObject({ active: true, sawChange: true });
    const done = operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", requestedAt: 600 });
    expect(done.reconcile).toMatchObject({ active: false, finished: true });
    expect(operationPhase(done)).toBe("complete");
    s = operationReducer(s, { type: "reconcileTimedOut" });
    expect(s.reconcile).toMatchObject({ active: false, timedOut: true });
    expect(operationReducer(s, { type: "dismissTimeout" }).reconcile.timedOut).toBe(false);
  });

  it("never finishes a detached run on the old CSV's Succeeded while the server may still run it (R4b a)", () => {
    let s = operationReducer(initialOperationState(), { type: "start", id: 1, kind: "update", source: "stream", now: 100, baseline: "rhods-operator.3.6.0|old|sha256:old" });
    s = operationReducer(s, { type: "bindServerId", id: 1, serverId: "op-1" });
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "detached", reason: "connection_lost", message: "lost" }, now: 500 });
    expect(s.reconcile).toMatchObject({ active: true, awaitingServer: true, result: "unknown", serverId: "op-1" });
    // The poll right after the drop sees the old operator, still Succeeded.
    const stale = operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", fingerprint: "rhods-operator.3.6.0|old|sha256:old", requestedAt: 600 });
    expect(stale.reconcile).toMatchObject({ active: true, finished: false, sawChange: false });
    // GET /api/operation no longer reports it; only a status requested after that counts.
    s = operationReducer(stale, { type: "serverSettled", now: 9_000, result: { success: true } });
    expect(s.reconcile).toMatchObject({ active: true, awaitingServer: false, result: "succeeded", evidenceAfter: 9_000 });
    expect(operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", requestedAt: 8_000 }).reconcile.active).toBe(true);
    const done = operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", fingerprint: "rhods-operator.3.6.0|new|sha256:new", requestedAt: 9_100 });
    expect(done.reconcile).toMatchObject({ active: false, finished: true, sawChange: true, result: "succeeded" });
  });

  it("turns a detached run into a failed one when the server reports the operation failed (N4)", () => {
    let s = operationReducer(started(), { type: "end", id: 1, outcome: { status: "detached", reason: "stalled", message: "x" }, now: 500 });
    s = operationReducer(s, { type: "serverSettled", now: 600, result: { success: false, message: "CSV failed; previous operator restored" } });
    expect(s.reconcile).toMatchObject({ active: false, finished: false, awaitingServer: false });
    expect(s.run?.outcome).toMatchObject({ status: "failed", message: "CSV failed; previous operator restored", rejected: false });
  });

  it("keeps the outcome unknown when the server settles without a result (older backend)", () => {
    let s = operationReducer(started(), { type: "end", id: 1, outcome: { status: "detached", reason: "stalled", message: "x" }, now: 500 });
    s = operationReducer(s, { type: "serverSettled", now: 600 });
    s = operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", requestedAt: 700 });
    expect(s.reconcile).toMatchObject({ finished: true, result: "unknown" });
  });

  it("server_lost does not wait for the server again, but its result stays unknown", () => {
    let s = operationReducer(initialOperationState(), { type: "start", id: 1, kind: "update", source: "server", now: 100, serverId: "op-9" });
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "detached", reason: "server_lost", message: "gone" }, now: 500 });
    expect(s.reconcile).toMatchObject({ active: true, awaitingServer: false, result: "unknown", serverId: "op-9" });
    expect(operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", requestedAt: 400 }).reconcile.active).toBe(true);
    expect(operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded", requestedAt: 500 }).reconcile.finished).toBe(true);
  });

  it("a new run supersedes the previous run's tracking", () => {
    const s = operationReducer(started(), { type: "end", id: 1, outcome: { status: "succeeded", message: "ok" }, now: 500 });
    const next = operationReducer(s, { type: "start", id: 2, kind: "refresh", source: "server", now: 600, serverId: "op-2" });
    expect(next.reconcile).toMatchObject({ active: false, finished: false, timedOut: false });
  });

  it("retargets a server run whose kind changed (N5) and binds a server id once", () => {
    let s = operationReducer(initialOperationState(), { type: "start", id: 1, kind: "reinstall_nightly", source: "server", now: 1, serverId: "op-3" });
    s = operationReducer(s, { type: "retarget", id: 1, kind: "reinstall_stable", detail: "stable" });
    expect(s.run).toMatchObject({ kind: "reinstall_stable", detail: "stable" });
    s = operationReducer(s, { type: "bindServerId", id: 1, serverId: "other" });
    expect(s.run?.serverId).toBe("op-3");
  });

  it("maps both reinstall kinds to reinstall reconcile tracking", () => {
    let s = operationReducer(initialOperationState(), { type: "start", id: 1, kind: "reinstall_nightly", source: "stream", now: 1 });
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "succeeded", message: "ok" }, now: 2 });
    expect(s.reconcile.kind).toBe("reinstall");
  });
});
