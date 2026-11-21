import { Profiler } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { App } from "./App";
import { nightlyStatus } from "./test/providers";
import { USER_PERMISSIONS } from "./test/providers";
import { BACKGROUND_POLL_MS, OPERATION_POLL_BUSY_MS, OPERATION_POLL_IDLE_MS } from "./constants";
import type { OperationStatusResponse } from "./types";

// How often an unchanged poll re-renders the page the user looks at.
// React's <Profiler> calls onRender once per commit in which anything
// inside it rendered (https://react.dev/reference/react/Profiler#onrender-callback).
// The page here reads no app state itself, so it renders again only when
// the app shell above it does. Each test asserts the polls and the page's
// commits together, so a failure shows both numbers.
const commits = vi.hoisted(() => ({ page: 0 }));
const countingPage = (name: string) => () => (
  <Profiler id={name} onRender={() => { commits.page += 1; }}>
    <h1>{name}</h1>
  </Profiler>
);
vi.mock("./pages/ComponentsPage", () => ({ ComponentsPage: countingPage("Components page") }));
vi.mock("./pages/UserStatusPage", () => ({ UserStatusPage: countingPage("User status page") }));

const ALICE: OperationStatusResponse = {
  inProgress: true,
  operation: {
    id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: "2026-10-05T10:00:00Z",
    target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64), step: "wait_catalog_ready", stepStatus: "running", message: "CatalogSource: CONNECTING",
  },
};

/** Every read answers the same JSON every time; `calls` counts them by path. */
function stubSameAnswers(answers: Record<string, unknown>) {
  const calls: Record<string, number> = {};
  vi.stubGlobal("fetch", vi.fn(async (input: string) => {
    const path = String(input).split("?")[0];
    calls[path] = (calls[path] ?? 0) + 1;
    const body = answers[path] ?? {};
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  }));
  return calls;
}

const tick = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

beforeEach(() => {
  // setImmediate stays real: openAndSettle lets the lazy page's import finish with it.
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
  commits.page = 0;
});
afterEach(() => {
  vi.useRealTimers();
  window.history.pushState({}, "", "/");
});

async function openAndSettle(path: string) {
  window.history.pushState({}, "", path);
  render(<App />);
  // The page's chunk loads (module I/O, not a timer): let real I/O run until it shows.
  for (let i = 0; i < 500 && !screen.queryByRole("heading", { level: 1 }); i++) {
    await act(async () => {
      await new Promise((r) => setImmediate(r));
      await vi.advanceTimersByTimeAsync(10);
    });
  }
  expect(screen.getByRole("heading", { level: 1 })).toBeInTheDocument();
  await tick(1_000);
}

describe("polls with unchanged answers", () => {
  it("cluster-admin, nothing running: operation polls every 15 s, status every 60 s, Dashboard Dev state every 120 s", async () => {
    const calls = stubSameAnswers({
      "/api/user/permissions": { canMutate: true, user: "me", role: "admin" },
      "/api/status": nightlyStatus(),
      "/api/operation": { inProgress: false, operation: null },
      "/api/version": { version: "dev", commit: "c", buildDate: "d", templateOutdated: false },
      "/api/dashboard/state": { currentImage: "", isCustomPR: false, managed: true, podStatus: "", podReady: true, containersReady: 1, containersTotal: 1, rolloutPending: false, canAssistRollout: false, override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false } },
    });
    await openAndSettle("/components");
    const at = (p: string) => calls[p] ?? 0;
    const before = { page: commits.page, operation: at("/api/operation"), status: at("/api/status"), dashboard: at("/api/dashboard/state") };
    // 16 idle operation polls; 4 status and 2 dashboard-state polls in the same 240 s.
    for (let i = 0; i < 16; i++) await tick(OPERATION_POLL_IDLE_MS);
    const polls = { operation: at("/api/operation") - before.operation, status: at("/api/status") - before.status, dashboard: at("/api/dashboard/state") - before.dashboard };
    expect({ ...polls, pageCommits: commits.page - before.page }).toEqual({ operation: 16, status: 4, dashboard: 2, pageCommits: 0 });
  });

  it("cluster-admin, a teammate's operation runs: operation polls every 3 s", async () => {
    const calls = stubSameAnswers({
      "/api/user/permissions": { canMutate: true, user: "me", role: "admin" },
      "/api/status": nightlyStatus(),
      "/api/operation": ALICE,
      "/api/version": { version: "dev", commit: "c", buildDate: "d", templateOutdated: false },
    });
    await openAndSettle("/components");
    const before = { page: commits.page, operation: calls["/api/operation"] };
    for (let i = 0; i < 10; i++) await tick(OPERATION_POLL_BUSY_MS);
    expect({ polls: calls["/api/operation"] - before.operation, pageCommits: commits.page - before.page }).toEqual({ polls: 10, pageCommits: 0 });
  });

  it("a user who is not a cluster-admin: operation polls every 15 s, no status", async () => {
    const calls = stubSameAnswers({
      "/api/user/permissions": USER_PERMISSIONS,
      "/api/operation": { running: false },
    });
    await openAndSettle("/");
    const before = { page: commits.page, operation: calls["/api/operation"] };
    for (let i = 0; i < 4; i++) await tick(OPERATION_POLL_IDLE_MS);
    expect({ polls: calls["/api/operation"] - before.operation, pageCommits: commits.page - before.page }).toEqual({ polls: 4, pageCommits: 0 });
    expect(calls["/api/status"]).toBeUndefined();
  });

  it("(the status poll's interval, for the numbers above)", () => {
    expect(BACKGROUND_POLL_MS).toBe(4 * OPERATION_POLL_IDLE_MS);
  });
});
