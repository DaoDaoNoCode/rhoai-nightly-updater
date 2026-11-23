import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator, terminalReason } from "./QuickResourceCreator";
import { TooltipButton } from "./TooltipButton";
import type { ResourcesStatus, ResourceState } from "../types";
import { jsonResponse, setDocumentHidden } from "../test/utils";

const notDeployed: ResourceState = { deployed: false, ready: false, message: "Not deployed" };

function statusWith(minio: ResourceState): ResourcesStatus {
  return { minio, mlflow: notDeployed, pipelineServers: [] };
}

function stubResources(get: () => ResourcesStatus, projects: string[] = []) {
  const fetchMock = vi.fn(async (url: string) => {
    if (url.includes("/api/resources/projects")) return jsonResponse({ projects });
    return jsonResponse(get());
  });
  vi.stubGlobal("fetch", fetchMock);
  return () => fetchMock.mock.calls.filter(([u]) => String(u).includes("/api/resources/status")).length;
}

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

function renderAt(path: string, mutateBlocker: string | null = null) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={mutateBlocker} /></MemoryRouter>);
}

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("terminalReason", () => {
  it.each([
    [{ deployed: true, ready: false, message: "ImagePullBackOff" }, "ImagePullBackOff"],
    [{ deployed: true, ready: false, waitingReason: "CrashLoopBackOff", message: "0/1 ready" }, "0/1 ready"],
    [{ deployed: true, ready: false, terminalError: true, waitingReason: "Unschedulable" }, "Unschedulable"],
    [{ deployed: true, ready: false, message: "Provisioning" }, null],
    [{ deployed: true, ready: true, message: "ImagePullBackOff" }, null],
  ])("%j -> %s", (state, expected) => {
    expect(terminalReason(state as ResourceState)).toBe(expected);
  });
});

describe("QuickResourceCreator polling", () => {
  it("backs off while a resource starts and stops once it is ready", async () => {
    let minio: ResourceState = { deployed: true, ready: false, message: "Provisioning" };
    const statusCalls = stubResources(() => statusWith(minio));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    expect(statusCalls()).toBe(1);
    await tick(5_000);
    expect(statusCalls()).toBe(2);
    await tick(5_000);
    expect(statusCalls()).toBe(2); // second interval is 10 s
    await tick(5_000);
    expect(statusCalls()).toBe(3);
    minio = { deployed: true, ready: true, message: "Running" };
    await tick(20_000);
    expect(statusCalls()).toBe(4);
    // Settled: the quick polling stops; only the slow idle re-read remains.
    await tick(44_000);
    expect(statusCalls()).toBe(4);
    await tick(1_000);
    expect(statusCalls()).toBe(5);
  });

  // N1: an idle page reads again on its own, slowly, only while visible,
  // at once when shown again, and never two reads at a time.
  it("an idle page re-reads every 45 s while visible, not while hidden, and at once when shown", async () => {
    const statusCalls = stubResources(() => statusWith(notDeployed));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    expect(statusCalls()).toBe(1);
    await tick(44_000);
    expect(statusCalls()).toBe(1);
    await tick(1_000);
    expect(statusCalls()).toBe(2);
    act(() => setDocumentHidden(true));
    await tick(10 * 60_000);
    expect(statusCalls()).toBe(2);
    act(() => setDocumentHidden(false));
    await tick();
    expect(statusCalls()).toBe(3);
  });

  it("an idle re-read waits for the one before it", async () => {
    let release: (() => void) | null = null;
    let calls = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url.includes("/api/resources/projects")) return jsonResponse({ projects: [] });
      if (!url.includes("/api/resources/status")) return jsonResponse({});
      calls += 1;
      if (calls === 2) await new Promise<void>((r) => { release = r; });
      return jsonResponse(statusWith(notDeployed));
    }));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    await tick(45_000);
    expect(calls).toBe(2);
    await tick(5 * 60_000);
    expect(calls).toBe(2);
    await act(async () => { release!(); });
    await tick(45_000);
    expect(calls).toBe(3);
  });

  // One poller: settling again after an earlier settling gave up starts the
  // quick reads again (its own 10 minutes), and the idle re-read does not
  // run beside them, so a read never starts while one is in flight.
  it("a new settling period after giving up polls quickly again, one read at a time", async () => {
    const provisioning: ResourceState = { deployed: true, ready: false, message: "Provisioning" };
    let minio = provisioning;
    let hang = false;
    let calls = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url.includes("/api/resources/projects")) return jsonResponse({ projects: [] });
      if (!url.includes("/api/resources/status")) return jsonResponse({});
      calls += 1;
      if (hang) await new Promise<void>(() => {});
      return jsonResponse(statusWith(minio));
    }));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    await tick(11 * 60_000);
    expect(screen.getByText("Stopped checking automatically after 10 minutes")).toBeInTheDocument();
    minio = { deployed: true, ready: true, message: "Running" };
    await tick(45_000);
    await tick();
    expect(screen.queryByText("Stopped checking automatically after 10 minutes")).toBeNull();
    minio = provisioning;
    await tick(45_000);
    const settlingAgain = calls;
    await tick(5_000);
    expect(calls).toBe(settlingAgain + 1);
    expect(screen.queryByText("Stopped checking automatically after 10 minutes")).toBeNull();
    hang = true;
    const before = calls;
    await tick(5 * 60_000);
    expect(calls).toBe(before + 1);
  });

  it("does not poll while the tab is hidden", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "Provisioning" }));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    act(() => setDocumentHidden(true));
    await tick(5 * 60_000);
    expect(statusCalls()).toBe(1);
    // Showing the tab polls at once (a state update): fire it inside act.
    act(() => setDocumentHidden(false));
    await tick();
    expect(statusCalls()).toBe(2);
  });

  it("stops polling and shows a failure for an image that can never be pulled", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "ImagePullBackOff" }));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    expect(within(screen.getByRole("listitem", { name: /^S3 storage/ })).getByText("Error")).toBeInTheDocument();
    // No quick settle polling for a terminal state: only the idle re-read, every 45 s.
    await tick(44_000);
    expect(statusCalls()).toBe(1);
    await tick(10 * 60_000 - 44_000);
    expect(statusCalls()).toBe(1 + Math.floor((10 * 60_000) / 45_000));
  });

  it("gives up after 10 minutes of settling and offers to check again", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "Provisioning" }));
    renderAt("/test-resources?tab=pipelines");
    await tick();
    await tick(11 * 60_000);
    const calls = statusCalls();
    // Given up: no more quick polling (30 s), only the idle re-read (45 s).
    await tick(30 * 60_000);
    expect(statusCalls() - calls).toBeLessThanOrEqual((30 * 60_000) / 45_000);
    expect(statusCalls() - calls).toBeGreaterThanOrEqual((30 * 60_000) / 45_000 - 1);
    expect(screen.getByText("Check again")).toBeInTheDocument();
    const before = statusCalls();
    fireEvent.click(screen.getByText("Check again"));
    await tick();
    expect(statusCalls()).toBe(before + 1);
  });
});

describe("disabled buttons explain themselves", () => {
  it("TooltipButton is focusable, shows the reason on focus and ignores clicks", async () => {
    const onClick = vi.fn();
    render(<TooltipButton onClick={onClick} disabledReason="You don't have permission">Dry Run</TooltipButton>);
    const button = screen.getByRole("button", { name: "Dry Run" });
    expect(button).toHaveAttribute("aria-disabled", "true");
    expect(button).not.toBeDisabled();
    fireEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
    act(() => button.focus());
    await tick(1000);
    expect(screen.getByRole("tooltip")).toHaveTextContent("You don't have permission");
  });

  it("Set up stays focusable and explains why it is disabled", async () => {
    const blocked = "Namespace 'seaweedfs' exists but was not created by this tool, so setup will not modify it.";
    stubResources(() => statusWith({ deployed: false, ready: false, managedByTool: false, setupBlockedReason: blocked }), ["dev-project"]);
    renderAt("/test-resources?tab=pipelines&project=dev-project");
    await tick();
    const setUp = within(screen.getByRole("tabpanel")).getByRole("button", { name: "Set up Pipelines in dev-project" });
    // Item 8: the reason is also under the button, not only in the tooltip.
    expect(setUp).toHaveAccessibleDescription(blocked);
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    expect(setUp).not.toBeDisabled();
    fireEvent.mouseEnter(setUp);
    await tick(1000);
    expect(screen.getByRole("tooltip")).toHaveTextContent(blocked);
    fireEvent.click(setUp);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("read-only users get the reason on the resource buttons", async () => {
    stubResources(() => statusWith(notDeployed), ["dev-project"]);
    renderAt("/test-resources?tab=pipelines&project=dev-project", "Read-only access: changing the cluster through this tool requires the cluster-admin role.");
    await tick();
    const setUp = screen.getAllByRole("button", { name: "Set up Pipelines in dev-project" })[0];
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    fireEvent.mouseEnter(setUp);
    await tick(1000);
    expect(screen.getByRole("tooltip")).toHaveTextContent(/cluster-admin/);
    fireEvent.click(setUp);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
