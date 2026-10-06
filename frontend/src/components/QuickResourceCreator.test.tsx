import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { QuickResourceCreator, terminalReason } from "./QuickResourceCreator";
import { TooltipButton } from "./TooltipButton";
import type { ResourcesStatus, ResourceState } from "../types";
import { jsonResponse, setDocumentHidden } from "../test/utils";

const notDeployed: ResourceState = { deployed: false, ready: false, message: "Not deployed" };

function statusWith(minio: ResourceState): ResourcesStatus {
  return { minio, mlflow: notDeployed, pipelineServers: [] };
}

function stubResources(get: () => ResourcesStatus) {
  const fetchMock = vi.fn(async (url: string) => {
    if (url.includes("/api/resources/projects")) return jsonResponse({ projects: [] });
    return jsonResponse(get());
  });
  vi.stubGlobal("fetch", fetchMock);
  return () => fetchMock.mock.calls.filter(([u]) => String(u).includes("/api/resources/status")).length;
}

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

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

describe("QuickResourceCreator polling (A06-5)", () => {
  it("backs off while a resource starts and stops once it is ready", async () => {
    let minio: ResourceState = { deployed: true, ready: false, message: "Provisioning" };
    const statusCalls = stubResources(() => statusWith(minio));
    render(<QuickResourceCreator mutateBlocker={null} />);
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
    await tick(10 * 60_000);
    expect(statusCalls()).toBe(4);
  });

  it("does not poll while the tab is hidden", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "Provisioning" }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    await tick();
    setDocumentHidden(true);
    await tick(5 * 60_000);
    expect(statusCalls()).toBe(1);
    setDocumentHidden(false);
    await tick();
    expect(statusCalls()).toBe(2);
  });

  it("stops polling and shows a failure for an image that can never be pulled", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "ImagePullBackOff" }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    await tick();
    expect(screen.getByText("Failed: ImagePullBackOff")).toBeInTheDocument();
    await tick(10 * 60_000);
    expect(statusCalls()).toBe(1);
  });

  it("gives up after 10 minutes of settling and offers to check again", async () => {
    const statusCalls = stubResources(() => statusWith({ deployed: true, ready: false, message: "Provisioning" }));
    render(<QuickResourceCreator mutateBlocker={null} />);
    await tick();
    await tick(11 * 60_000);
    const calls = statusCalls();
    await tick(30 * 60_000);
    expect(statusCalls()).toBe(calls);
    fireEvent.click(screen.getByText("Check again"));
    await tick();
    expect(statusCalls()).toBe(calls + 1);
  });
});

describe("disabled buttons explain themselves (A06-7)", () => {
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

  it("'Add to a project' stays focusable and explains why it is disabled (N10)", async () => {
    stubResources(() => statusWith(notDeployed));
    render(<QuickResourceCreator mutateBlocker={null} />);
    await tick();
    const toggle = screen.getByRole("button", { name: "Add to a project" });
    expect(toggle).toHaveAttribute("aria-disabled", "true");
    expect(toggle).not.toBeDisabled();
    fireEvent.mouseEnter(toggle);
    await tick(1000);
    expect(screen.getByRole("tooltip")).toHaveTextContent("Available once the S3 storage is running.");
    fireEvent.click(toggle);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("read-only users get the reason on the resource buttons", async () => {
    stubResources(() => statusWith(notDeployed));
    render(<QuickResourceCreator mutateBlocker="Read-only access: changing the cluster through this tool requires the cluster-admin role." />);
    await tick();
    const setUp = screen.getAllByRole("button", { name: "Set up" })[0];
    expect(setUp).toHaveAttribute("aria-disabled", "true");
    fireEvent.mouseEnter(setUp);
    await tick(1000);
    expect(screen.getByRole("tooltip")).toHaveTextContent(/cluster-admin/);
    fireEvent.click(setUp);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
