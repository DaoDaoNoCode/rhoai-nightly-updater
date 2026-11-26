import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppStateProvider } from "./AppState";
import { LiveAnnouncerProvider } from "./LiveAnnouncer";
import { OPERATION_POLL_BUSY_MS, OPERATION_POLL_IDLE_MS } from "../constants";
import type { StatusResponse, UserOperationStatus } from "../types";

const status = { cluster: { server: "s", version: "4.19", user: "me" } } as unknown as StatusResponse;
const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

// While project actions show, GET /api/operation is polled at the busy
// interval, so their banners and Cancel follow them; once none runs, at
// the idle one again.
describe("polling while project actions run", () => {
  it("polls at the busy interval while a project action shows, then at the idle one", async () => {
    const running: UserOperationStatus = { running: false, operations: [{ scope: "project", label: "Set up the workbench", startedAt: "2026-11-20T14:05:00Z", project: "team-a" }] };
    const idle: UserOperationStatus = { running: false, operations: [] };
    const answers = [running, running, idle];
    const fetchOperation = vi.fn(async () => answers.shift() ?? idle);
    render(
      <LiveAnnouncerProvider>
        <MemoryRouter>
          <AppStateProvider fetchStatus={async () => status} fetchOperation={fetchOperation}>
            <p>app</p>
          </AppStateProvider>
        </MemoryRouter>
      </LiveAnnouncerProvider>,
    );
    await tick();
    expect(fetchOperation).toHaveBeenCalledTimes(1);
    await tick(OPERATION_POLL_BUSY_MS);
    expect(fetchOperation).toHaveBeenCalledTimes(2);
    await tick(OPERATION_POLL_BUSY_MS);
    expect(fetchOperation).toHaveBeenCalledTimes(3);
    // Nothing runs any more: the next read waits for the idle interval.
    await tick(OPERATION_POLL_BUSY_MS);
    expect(fetchOperation).toHaveBeenCalledTimes(3);
    await tick(OPERATION_POLL_IDLE_MS - OPERATION_POLL_BUSY_MS);
    expect(fetchOperation).toHaveBeenCalledTimes(4);
  });
});
