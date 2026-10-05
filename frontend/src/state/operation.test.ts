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
    expect(operationReducer(s, { type: "statusPolled", csvPhase: "Installing" })).toBe(s);
    const done = operationReducer(s, { type: "statusPolled", csvPhase: "Succeeded" });
    expect(done.reconcile).toMatchObject({ active: false, finished: true });
    expect(operationPhase(done)).toBe("complete");
    s = operationReducer(s, { type: "reconcileTimedOut" });
    expect(s.reconcile).toMatchObject({ active: false, timedOut: true });
    expect(operationReducer(s, { type: "dismissTimeout" }).reconcile.timedOut).toBe(false);
  });

  it("maps both reinstall kinds to reinstall reconcile tracking", () => {
    let s = operationReducer(initialOperationState(), { type: "start", id: 1, kind: "reinstall_nightly", source: "stream", now: 1 });
    s = operationReducer(s, { type: "end", id: 1, outcome: { status: "succeeded", message: "ok" }, now: 2 });
    expect(s.reconcile.kind).toBe("reinstall");
  });
});
