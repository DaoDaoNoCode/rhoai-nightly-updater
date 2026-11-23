import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { jsonResponse, setDocumentHidden } from "../../test/utils";
import { useMaaSStatus } from "./MaaSTab";
import { usePrerequisites } from "./Prerequisites";

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

function stub(path: string, body: unknown) {
  const fetchMock = vi.fn(async (_url: string) => jsonResponse(body));
  vi.stubGlobal("fetch", fetchMock);
  return () => fetchMock.mock.calls.filter(([u]) => String(u).includes(path)).length;
}

beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

// N1: MaaS and the prerequisites are read again slowly while nothing
// settles, only while the browser tab is visible, and at once when shown.
describe("idle re-reads", () => {
  it("MaaS: every 45 s, not while hidden, at once when shown", async () => {
    const reads = stub("/api/resources/maas", { platform: { deployed: false, ready: false, managedByTool: false }, parts: [], subscription: "s" });
    await act(async () => { renderHook(() => useMaaSStatus(0, false, true)); });
    await tick();
    expect(reads()).toBe(1);
    await tick(44_000);
    expect(reads()).toBe(1);
    await tick(1_000);
    expect(reads()).toBe(2);
    act(() => setDocumentHidden(true));
    await tick(10 * 60_000);
    expect(reads()).toBe(2);
    await act(async () => { setDocumentHidden(false); });
    await tick();
    expect(reads()).toBe(3);
  });

  it("Prerequisites: every 45 s", async () => {
    const reads = stub("/api/resources/prerequisites", { features: {} });
    await act(async () => { renderHook(() => usePrerequisites(0, null, true)); });
    await tick();
    expect(reads()).toBe(1);
    await tick(45_000);
    expect(reads()).toBe(2);
  });

  it("nothing is read while the read is off", async () => {
    const reads = stub("/api/resources/maas", {});
    await act(async () => { renderHook(() => useMaaSStatus(0, false, false)); });
    await tick(10 * 60_000);
    expect(reads()).toBe(0);
  });
});
