import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { stubApi } from "../../test/apiStub";
import type { ResourceState } from "../../types";
import { envStatus, notManaged } from "./resourceStatus";
import { READY_CHECK_MS, READY_TIMEOUT_MS, useGoalRun } from "./useGoalRun";
import { S3SecretField, copyFetchedText, s3EnvText } from "./AdminLogin";
import { storageNotReadyReason } from "./featureState";

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
    // Health, not ownership: someone else's resource reads what it is.
    ["someone else's, running", { ...base, ready: true, managedByTool: false }, {}, "ready"],
    ["someone else's, broken", { ...base, managedByTool: false, terminalError: true, waitingReason: "CrashLoopBackOff" }, {}, "error"],
    ["someone else's, needs a repair the tool does not make", { ...base, managedByTool: false, terminalError: true, repairNeeded: "x", message: "x" }, {}, "error"],
    ["a foreign namespace blocks setup", { deployed: false, ready: false, managedByTool: false, setupBlockedReason: "x" }, {}, "not-set-up"],
    ["data to migrate", { ...base, ready: true }, { attention: true }, "needs-attention"],
    ["this page sets it up", { deployed: false, ready: false }, { busy: "setting-up" }, "setting-up"],
  ])("%s", (_name, state, opts, want) => {
    expect(envStatus(state, opts)).toBe(want);
  });

  it("ownership is apart from health, and only for something that exists", () => {
    expect(notManaged({ deployed: true, ready: true, managedByTool: false })).toBe(true);
    expect(notManaged({ deployed: true, ready: true, managedByTool: true })).toBe(false);
    expect(notManaged({ deployed: false, ready: false, managedByTool: false })).toBe(false);
    expect(notManaged({ deployed: false, ready: false, managedByTool: false, setupBlockedReason: "x" })).toBe(true);
  });

  it("pipeline servers wait for the storage and for a migration's copy (follow-up 3)", () => {
    expect(storageNotReadyReason({ deployed: true, ready: true, managedByTool: false }, null, false)).toMatch(/was not created by this tool/);
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
    expect(result.current.run?.result).toEqual({ success: true, message: "All 2 parts are done." });
    expect(result.current.run?.states.a.status).toBe("success");
    expect(result.current.run?.states.a.message).toMatch(/^A done \(\d+:\d\d\)$/);
  });

  // Item 1: a step succeeds only when its part works.
  const fakeClock = () => {
    let t = 0;
    return { now: () => t, sleep: async (ms: number) => { t += ms; } };
  };
  it("waits until the status says the part works, with the elapsed time", async () => {
    const c = fakeClock();
    let checks = 0;
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [{
      id: "a", title: "A", run: ok("created"), readyMessage: "A is ready.",
      waitReady: { typical: "1 to 3 minutes", check: () => { checks += 1; return checks < 4 ? { state: "starting" } : { state: "ready" }; } },
    }], { doneMessage: "Goal is ready." }));
    expect(result.current.run?.result).toEqual({ success: true, message: "Goal is ready." });
    expect(result.current.run?.states.a).toEqual({ status: "success", message: "A is ready. (0:06)" });
  });
  it("reads the status again before each later check, so a check never judges a status nobody reads any more", async () => {
    const c = fakeClock();
    const seen: string[] = [];
    let latest = "starting";
    let reads = 0;
    const refresh = vi.fn(async () => { seen.push("refresh"); reads += 1; latest = reads >= 2 ? "ready" : "starting"; });
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [{
      id: "a", title: "A", run: ok("created"),
      waitReady: { typical: "x", check: () => { seen.push(`check ${latest}`); return latest === "ready" ? { state: "ready" } : { state: "starting" }; } },
    }], { refresh }));
    expect(seen).toEqual(["check starting", "refresh", "check starting", "refresh", "check ready"]);
    expect(result.current.run?.result).toMatchObject({ success: true });
  });
  it("a status read that fails for good (the session ended) stops the run at that part; no read after the waiting time is up", async () => {
    const c = fakeClock();
    const next = vi.fn(ok("B"));
    const left: number[] = [];
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [
      { id: "a", title: "A", run: ok("created"), waitReady: { typical: "x", check: () => ({ state: "starting" }) } },
      { id: "b", title: "B", run: next },
    ], { refresh: async (remainingMs) => { left.push(remainingMs); if (left.length === 2) throw Object.assign(new Error("Your OpenShift session has expired."), { errorCode: "session_expired" }); } }));
    expect(left).toEqual([READY_TIMEOUT_MS - READY_CHECK_MS, READY_TIMEOUT_MS - 2 * READY_CHECK_MS]);
    expect(result.current.run?.states.a.status).toBe("failed");
    expect(result.current.run?.result).toMatchObject({ success: false, message: "Your OpenShift session has expired." });
    expect(next).not.toHaveBeenCalled();
  });
  it("a part found ready only by a read that ended past the waiting time does not start the next change", async () => {
    const c = fakeClock();
    const next = vi.fn(ok("B"));
    let read = false;
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [
      { id: "a", title: "A", run: ok("created"), waitReady: { typical: "x", check: () => (read ? { state: "ready" } : { state: "starting" }) } },
      { id: "b", title: "B", run: next },
    ], { refresh: async (remainingMs) => { await c.sleep(remainingMs + 1); read = true; } }));
    expect(next).not.toHaveBeenCalled();
    expect(result.current.run?.result).toMatchObject({ success: false, errorCode: "in_progress" });
  });
  it("a part that cannot start fails the run with the reason and the fix", async () => {
    const c = fakeClock();
    const next = vi.fn(ok("B"));
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [
      { id: "a", title: "A", run: ok("created"), waitReady: { typical: "x", check: () => ({ state: "failed", reason: "It cannot start: ImagePullBackOff. Repair applies it again." }) } },
      { id: "b", title: "B", run: next },
    ]));
    expect(result.current.run?.result).toEqual({ success: false, message: "It cannot start: ImagePullBackOff. Repair applies it again." });
    expect(next).not.toHaveBeenCalled();
  });
  it("gives up waiting after 10 minutes, as still starting, not as a failure", async () => {
    const c = fakeClock();
    const { result } = renderHook(() => useGoalRun(c.now, c.sleep));
    await act(() => result.current.start("Goal", [{ id: "a", title: "A", run: ok("created"), waitReady: { typical: "x", check: () => ({ state: "starting" }) } }]));
    expect(result.current.run?.result).toMatchObject({ success: false, errorCode: "in_progress" });
    expect(result.current.run?.result?.message).toMatch(/created, but not ready after 10:00/);
    expect(result.current.run?.states.a.status).toBe("running");
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

  // Item 3: every part's prerequisites are checked before the first part is created.
  it("a preflight that finds a missing prerequisite changes nothing", async () => {
    const first = vi.fn(ok("A"));
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Goal", [{ id: "a", title: "A", run: first }, { id: "b", title: "B", run: ok("B") }],
      { preflight: async () => "Data Science Pipelines is turned off on this cluster." }));
    expect(first).not.toHaveBeenCalled();
    expect(result.current.run?.steps.map((s) => s.id)).toEqual(["check", "a", "b"]);
    expect(result.current.run?.result).toEqual({ success: false, message: "Data Science Pipelines is turned off on this cluster. Nothing was changed.", errorCode: "prerequisites" });
  });
  it("a preflight that passes runs the steps after its own", async () => {
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Goal", [{ id: "a", title: "A", run: ok("A") }], { preflight: async () => null, kind: "setup" }));
    expect(result.current.run?.states.check.status).toBe("success");
    expect(result.current.run?.result?.success).toBe(true);
  });
  it("an operation still in progress keeps its step running, not failed", async () => {
    const { result } = renderHook(() => useGoalRun());
    await act(() => result.current.start("Delete", [{ id: "a", title: "A", run: async () => ({ success: false, message: "still terminating", logs: [], errorCode: "in_progress" }) }], { kind: "delete" }));
    expect(result.current.run?.states.a.status).toBe("running");
    expect(result.current.run?.result?.errorCode).toBe("in_progress");
  });

  // Item 4: Clean up is one streamed operation; each event updates its step.
  it("follows a streamed operation step by step to its outcome", () => {
    const { result } = renderHook(() => useGoalRun());
    let emit: (s: { step: string; status: "running" | "success" | "failed" | "skipped"; message: string; elapsedMs: number }) => void = () => {};
    let done: (success: boolean, error?: string) => void = () => {};
    act(() => result.current.follow("Clean up", [{ id: "pipeline-server:a", title: "Pipeline server in a" }, { id: "storage", title: "S3 storage" }], (onStep, onDone) => {
      emit = onStep;
      done = onDone;
      return new AbortController();
    }));
    act(() => emit({ step: "pipeline-server:a", status: "success", message: "Pipeline server removed from 'a'.", elapsedMs: 1 }));
    act(() => emit({ step: "storage", status: "failed", message: "Cannot delete the S3 storage yet.", elapsedMs: 2 }));
    act(() => emit({ step: "operation_complete", status: "failed", message: "Cannot delete the S3 storage yet. Deleted before it stopped: a.", elapsedMs: 3 }));
    act(() => done(false, "Cannot delete the S3 storage yet. Deleted before it stopped: a."));
    expect(result.current.run?.states["pipeline-server:a"].status).toBe("success");
    expect(result.current.run?.states.storage.status).toBe("failed");
    expect(result.current.run).toMatchObject({ kind: "delete", running: false, result: { success: false, message: "Cannot delete the S3 storage yet. Deleted before it stopped: a." } });
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

describe("S3SecretField", () => {
  it("for a user who cannot read it, both buttons say why and stay focusable", async () => {
    render(<S3SecretField id="k" field="password" label="S3 secret key" disabledReason="Read-only access: changing the cluster through this tool requires the cluster-admin role." />);
    const show = screen.getByRole("button", { name: "Show the S3 secret key" });
    expect(show).toHaveAttribute("aria-disabled", "true");
    expect(show).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Copy the S3 secret key" })).toHaveAttribute("aria-disabled", "true");
    fireEvent.mouseEnter(show);
    expect((await screen.findByRole("tooltip")).textContent).toMatch(/cluster-admin/);
    expect(screen.getByLabelText("S3 secret key")).toHaveAttribute("type", "password");
  });
});

describe("S3SecretField: the admin UI password", () => {
  it("is the password of its own, not the S3 secret key", async () => {
    stubApi({ "/api/resources/s3/admin-password": { user: "admin", password: "AP", accessKey: "AK", secretKey: "SK" } });
    render(<S3SecretField id="p" field="password" label="admin UI password" disabledReason={null} />);
    fireEvent.click(screen.getByRole("button", { name: "Show the admin UI password" }));
    await waitFor(() => expect(screen.getByLabelText("admin UI password")).toHaveValue("AP"));
  });
  it("on storage an earlier version set up, says Repair sets one", async () => {
    stubApi({ "/api/resources/s3/admin-password": { user: "admin", password: "", accessKey: "AK", secretKey: "SK" } });
    render(<S3SecretField id="p" field="password" label="admin UI password" disabledReason={null} />);
    fireEvent.click(screen.getByRole("button", { name: "Show the admin UI password" }));
    expect(await screen.findByText(/There is no admin UI password yet. Repair the S3 storage/)).toBeInTheDocument();
    expect(screen.getByLabelText("admin UI password")).toHaveAttribute("type", "password");
  });
});

describe("s3EnvText", () => {
  it("is what an S3 client reads, with the endpoint and both keys", () => {
    expect(s3EnvText("http://seaweedfs-s3.seaweedfs.svc:8333", { accessKey: "AK", secretKey: "SK" })).toBe(
      "AWS_ENDPOINT_URL='http://seaweedfs-s3.seaweedfs.svc:8333'\nAWS_ACCESS_KEY_ID='AK'\nAWS_SECRET_ACCESS_KEY='SK'\nAWS_DEFAULT_REGION='us-east-1'\nAWS_S3_BUCKET='pipelines'\n");
  });

  it("single-quotes every value, so a pasted key never runs or splits", () => {
    const text = s3EnvText("http://s3:8333", { accessKey: "a b", secretKey: "x$(id)`y`'z" });
    expect(text).toContain("AWS_ACCESS_KEY_ID='a b'\n");
    expect(text).toContain("AWS_SECRET_ACCESS_KEY='x$(id)`y`'\\''z'\n");
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
