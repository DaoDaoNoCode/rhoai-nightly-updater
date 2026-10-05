import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { LABELS_FIRST_PAINT_MS, mergeDeploymentLabels, extractLabels, useComponentsData } from "./useComponentsData";
import type { ComponentsResponse, DeploymentInfo } from "../types";
import { jsonResponse } from "../test/utils";

function dep(name: string, image: string, ready: number, labels = false): DeploymentInfo {
  return {
    name, namespace: "redhat-ods-applications", ready, desired: 1, available: ready, image,
    unavailableReplicas: 1 - ready, updatedReplicas: 1, rolloutStuck: false,
    ...(labels ? { gitCommit: `sha-${image}`, gitURL: "https://github.com/x/y", buildDate: "2026-10-01T00:00:00Z", version: "v1" } : {}),
  };
}

function response(deployments: DeploymentInfo[], dscPhase = "Ready"): ComponentsResponse {
  return { components: [], deployments, dscName: "default-dsc", dscPhase, changedCount: 0 };
}

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("mergeDeploymentLabels", () => {
  it("overlays labels only for the same deployment and image", () => {
    const labels = extractLabels(response([dep("a", "img1", 1, true), dep("b", "img1", 1, true)]));
    const merged = mergeDeploymentLabels([dep("a", "img1", 0), dep("b", "img2", 0)], labels);
    expect(merged[0]).toMatchObject({ ready: 0, gitCommit: "sha-img1" });
    // b rolled out to a new image: its old labels must not be shown.
    expect(merged[1].gitCommit).toBeUndefined();
  });
});

describe("useComponentsData", () => {
  it("shows every poll in the deployments table and keeps the labels (A06-2)", async () => {
    const plain = [response([dep("a", "img1", 0)], "Polled")];
    const fetchMock = vi.fn(async (url: string) =>
      url.includes("labels=true") ? jsonResponse(response([dep("a", "img1", 1, true)])) : jsonResponse(plain[0]),
    );
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useComponentsData());
    await tick();
    // Warm label cache: one request paints the page with labels.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(result.current.data?.deployments[0]).toMatchObject({ ready: 1, gitCommit: "sha-img1" });

    await tick(30_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(result.current.data?.dscPhase).toBe("Polled");
    expect(result.current.data?.deployments[0]).toMatchObject({ ready: 0, gitCommit: "sha-img1" });
  });

  it("paints with a plain request when labels are slow, without letting the late labels response roll the data back", async () => {
    let releaseLabels: (() => void) | undefined;
    const fetchMock = vi.fn((url: string) => {
      if (url.includes("labels=true")) {
        return new Promise<Response>((resolve) => {
          releaseLabels = () => resolve(jsonResponse(response([dep("a", "img1", 1, true)], "Old")));
        });
      }
      return Promise.resolve(jsonResponse(response([dep("a", "img1", 0)], "New")));
    });
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useComponentsData());
    await tick(LABELS_FIRST_PAINT_MS);
    await tick();
    expect(result.current.data?.dscPhase).toBe("New");
    expect(result.current.labelsLoading).toBe(true);
    releaseLabels?.();
    await tick();
    // The labels response started first, so its data is older; only its labels are used.
    expect(result.current.data?.dscPhase).toBe("New");
    expect(result.current.data?.deployments[0]).toMatchObject({ ready: 0, gitCommit: "sha-img1" });
  });

  it("fetches labels again only when a poll shows new images", async () => {
    let image = "img1";
    const fetchMock = vi.fn(async (url: string) =>
      url.includes("labels=true") ? jsonResponse(response([dep("a", image, 1, true)])) : jsonResponse(response([dep("a", image, 1)])),
    );
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useComponentsData());
    await tick();
    await tick(30_000);
    await tick(30_000);
    const labelCalls = () => fetchMock.mock.calls.filter(([u]) => String(u).includes("labels=true")).length;
    expect(labelCalls()).toBe(1);
    image = "img2";
    await tick(30_000);
    await tick();
    expect(labelCalls()).toBe(2);
    expect(result.current.data?.deployments[0].gitCommit).toBe("sha-img2");
  });
});
