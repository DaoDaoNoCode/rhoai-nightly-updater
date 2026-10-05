import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { createPoller, exponentialBackoff, usePolling } from "./usePolling";
import { useAsyncData } from "./useAsyncData";
import { setDocumentHidden } from "../test/utils";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("createPoller", () => {
  it("polls on a fixed interval, one request at a time", async () => {
    let resolveSlow: (() => void) | undefined;
    const task = vi.fn(() => new Promise<void>((r) => { resolveSlow = r; }));
    const poller = createPoller({ task, delay: 1000 });
    poller.start();
    await vi.advanceTimersByTimeAsync(1000);
    expect(task).toHaveBeenCalledTimes(1);
    // The first request is still in flight: no overlapping poll.
    await vi.advanceTimersByTimeAsync(5000);
    expect(task).toHaveBeenCalledTimes(1);
    resolveSlow?.();
    await vi.advanceTimersByTimeAsync(1000);
    expect(task).toHaveBeenCalledTimes(2);
    poller.stop();
  });

  it("skips polls while hidden and polls at once when the tab is visible again", async () => {
    const task = vi.fn(async () => {});
    const poller = createPoller({ task, delay: 1000 });
    poller.start();
    setDocumentHidden(true);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(task).not.toHaveBeenCalled();
    setDocumentHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(task).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(task).toHaveBeenCalledTimes(2);
    poller.stop();
  });

  it("backs off and stops when the delay function returns null", async () => {
    const task = vi.fn(async () => {});
    const delays: number[] = [];
    const poller = createPoller({
      task,
      delay: ({ attempt, elapsedMs }) => {
        if (elapsedMs >= 20_000) return null;
        const d = exponentialBackoff(attempt, 1000, 8000);
        delays.push(d);
        return d;
      },
    });
    poller.start();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(delays.slice(0, 5)).toEqual([1000, 2000, 4000, 8000, 8000]);
    const calls = task.mock.calls.length;
    expect(poller.finished).toBe(true);
    await vi.advanceTimersByTimeAsync(60_000);
    setDocumentHidden(true);
    setDocumentHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(task).toHaveBeenCalledTimes(calls);
    poller.stop();
  });

  it("reset restarts the backoff", async () => {
    const task = vi.fn(async () => {});
    const poller = createPoller({ task, delay: ({ attempt }) => exponentialBackoff(attempt, 1000, 60_000) });
    poller.start();
    await vi.advanceTimersByTimeAsync(1000 + 2000 + 4000);
    expect(task).toHaveBeenCalledTimes(3);
    poller.reset();
    await vi.advanceTimersByTimeAsync(1000);
    expect(task).toHaveBeenCalledTimes(4);
    poller.stop();
  });

  it("a task that settles after stop does not reschedule", async () => {
    let finish: (() => void) | undefined;
    const task = vi.fn(() => new Promise<void>((r) => { finish = r; }));
    const poller = createPoller({ task, delay: 1000 });
    poller.start();
    await vi.advanceTimersByTimeAsync(1000);
    poller.stop();
    finish?.();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(task).toHaveBeenCalledTimes(1);
  });
});

describe("usePolling", () => {
  it("does not restart when the caller passes a new inline task", async () => {
    let calls = 0;
    const { rerender, unmount } = renderHook(({ n }) => usePolling(() => { calls += n; }, { delay: 1000 }), { initialProps: { n: 1 } });
    await vi.advanceTimersByTimeAsync(1000);
    rerender({ n: 10 });
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(11);
    unmount();
    await vi.advanceTimersByTimeAsync(5000);
    expect(calls).toBe(11);
  });
});

describe("useAsyncData", () => {
  it("pauses polling in hidden tabs and refreshes on return", async () => {
    const fetchFn = vi.fn(async () => "ok");
    renderHook(() => useAsyncData(fetchFn, 30_000));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchFn).toHaveBeenCalledTimes(1); // mount
    setDocumentHidden(true);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchFn).toHaveBeenCalledTimes(1);
    setDocumentHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });

  it("keeps the last data and exposes a typed error when a poll fails", async () => {
    let fail = false;
    const fetchFn = vi.fn(async () => {
      if (fail) throw new TypeError("Failed to fetch");
      return { value: 1 };
    });
    const { result } = renderHook(() => useAsyncData(fetchFn, 1000));
    await vi.advanceTimersByTimeAsync(0);
    expect(result.current.data).toEqual({ value: 1 });
    fail = true;
    await vi.advanceTimersByTimeAsync(1000);
    expect(result.current.data).toEqual({ value: 1 });
    await vi.advanceTimersByTimeAsync(0);
    expect(result.current.error?.errorCode).toBe("network");
  });
});
