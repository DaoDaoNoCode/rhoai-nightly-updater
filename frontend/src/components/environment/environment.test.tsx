import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { ResourceState } from "../../types";
import { envStatus } from "./resourceStatus";
import { useGoalRun } from "./useGoalRun";
import { copyFetchedText } from "./CopyFromServerButton";
import { storageNotReadyReason } from "./StorageCard";

describe("status vocabulary", () => {
  const base: ResourceState = { deployed: true, ready: false, managedByTool: true };
  it.each<[string, ResourceState | undefined, Parameters<typeof envStatus>[1], ReturnType<typeof envStatus>]>([
    ["nothing", { deployed: false, ready: false, managedByTool: false }, {}, "not-set-up"],
    ["ready", { ...base, ready: true }, {}, "ready"],
    ["ready but degraded", { ...base, ready: true, repairNeeded: "Service points elsewhere" }, {}, "needs-attention"],
    ["starting", { ...base, message: "0/1 ready" }, {}, "setting-up"],
    ["cannot start", { ...base, terminalError: true, waitingReason: "ImagePullBackOff" }, {}, "error"],
    ["cannot start, Repair fixes it", { ...base, terminalError: true, repairNeeded: "scaled to 0", message: "scaled to 0" }, {}, "needs-attention"],
    ["terminating", { ...base, terminating: true }, {}, "deleting"],
    ["someone else's", { ...base, ready: true, managedByTool: false }, {}, "not-managed"],
    ["a foreign namespace blocks setup", { deployed: false, ready: false, managedByTool: false, setupBlockedReason: "x" }, {}, "not-managed"],
    ["data to migrate", { ...base, ready: true }, { attention: true }, "needs-attention"],
    ["this page sets it up", { deployed: false, ready: false }, { busy: "setting-up" }, "setting-up"],
  ])("%s", (_name, state, opts, want) => {
    expect(envStatus(state, opts)).toBe(want);
  });

  it("pipeline servers wait for the storage and for a migration's copy (follow-up 3)", () => {
    const ready: ResourceState = { deployed: true, ready: true };
    expect(storageNotReadyReason(ready, null, false)).toBeNull();
    expect(storageNotReadyReason(undefined, null, false)).toMatch(/once the S3 storage is set up/);
    expect(storageNotReadyReason({ deployed: true, ready: false }, null, false)).toMatch(/once the S3 storage is ready/);
    expect(storageNotReadyReason(ready, { stage: "migrate" } as never, false)).toMatch(/once Migrate storage has moved the data/);
    expect(storageNotReadyReason(ready, { stage: "remove" } as never, false)).toBeNull();
    expect(storageNotReadyReason(ready, null, true)).toMatch(/Migrate storage is running/);
  });
});

describe("useGoalRun", () => {
  const ok = (message: string) => async () => ({ success: true, message, logs: [] });
  it("runs the steps in order and reports each", async () => {
    const order: string[] = [];
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Goal", [
      { id: "a", title: "A", run: async () => { order.push("a"); return ok("A done")(); } },
      { id: "b", title: "B", run: async () => { order.push("b"); return ok("B done")(); } },
    ]));
    expect(order).toEqual(["a", "b"]);
    expect(result.current.run?.running).toBe(false);
    expect(result.current.run?.result).toEqual({ success: true, message: "All 2 parts are set up." });
    expect(result.current.run?.states.a.status).toBe("success");
    expect(result.current.run?.states.a.message).toMatch(/^A done \(took \d+ s\)$/);
  });

  it("stops at the first step that fails; later steps do not run", async () => {
    const second = vi.fn(ok("B"));
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Goal", [
      { id: "a", title: "A", run: async () => ({ success: false, message: "Storage refused", logs: [], errorCode: "not_managed" }) },
      { id: "b", title: "B", run: second },
    ]));
    expect(second).not.toHaveBeenCalled();
    expect(result.current.run?.result).toEqual({ success: false, message: "Storage refused", errorCode: "not_managed" });
    expect(result.current.run?.states.b).toBeUndefined();
  });

  it("does nothing without steps or while a run is going; reset keeps a running run", async () => {
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Goal", []));
    expect(result.current.run).toBeNull();
    let finish: (v: { success: boolean; message: string; logs: string[] }) => void = () => {};
    const slow = vi.fn(() => new Promise<{ success: boolean; message: string; logs: string[] }>((r) => { finish = r; }));
    let first: Promise<void> = Promise.resolve();
    act(() => { first = result.current.start("Goal", [{ id: "a", title: "A", run: slow }]); });
    await act(() => result.current.start("Goal", [{ id: "a", title: "A", run: slow }]));
    expect(slow).toHaveBeenCalledTimes(1);
    act(() => result.current.reset());
    expect(result.current.run?.running).toBe(true);
    await act(async () => { finish({ success: true, message: "ok", logs: [] }); await first; });
    act(() => result.current.reset());
    expect(result.current.run).toBeNull();
  });
});

describe("copyFetchedText", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });
  it("writes through writeText where ClipboardItem is missing", async () => {
    vi.stubGlobal("ClipboardItem", undefined);
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await copyFetchedText(async () => "pw");
    expect(writeText).toHaveBeenCalledWith("pw");
  });
  it("reports the request's error, not the clipboard's, when the request fails", async () => {
    class Item { constructor(public data: Record<string, Promise<Blob>>) {} }
    vi.stubGlobal("ClipboardItem", Item);
    const write = vi.fn(async (items: Item[]) => { await items[0].data["text/plain"]; });
    Object.defineProperty(navigator, "clipboard", { value: { write }, configurable: true });
    await expect(copyFetchedText(async () => { throw new Error("Only cluster-admins can read this through the updater."); })).rejects.toThrow(/Only cluster-admins/);
  });
});
