import { describe, expect, it } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { useClusterStatus } from "../state/AppState";
import { ADMIN_PERMISSIONS, nightlyStatus, renderWithApp } from "./providers";
import type { StatusResponse } from "../types";

// renderWithApp hands the providers' AbortSignal to the injected reads, as
// the app hands it to fetch (services/api): a test can see a read aborted
// when the app unmounts or a newer read of the same kind starts
// (hooks/useLatestRequest).

/** A read that answers when the test says and, like fetch, rejects with an AbortError once its signal aborts. */
function heldReads<T>() {
  const calls: Array<{ signal?: AbortSignal; resolve: (v: T) => void }> = [];
  const read = (signal?: AbortSignal) =>
    new Promise<T>((resolve, reject) => {
      calls.push({ signal, resolve });
      signal?.addEventListener("abort", () => reject(new DOMException("The operation was aborted.", "AbortError")));
    });
  return { calls, read };
}

const Probe = () => {
  const { status, refresh } = useClusterStatus();
  return (
    <>
      <button type="button" onClick={() => void refresh()}>refresh</button>
      <p data-testid="server">{status?.cluster.server ?? "-"}</p>
    </>
  );
};

describe("renderWithApp's injected reads", () => {
  it("receive the AbortSignal the providers pass", async () => {
    const seen: Record<string, unknown[]> = {};
    const record = <T,>(name: string, answer: T) => async (...args: unknown[]) => {
      (seen[name] ??= []).push(...args);
      return answer;
    };
    const { settled } = renderWithApp(<Probe />, {
      status: record("status", nightlyStatus()),
      version: record("version", { version: "dev", commit: "x", buildDate: "x", templateOutdated: false }),
      dashboard: record("dashboard", { override: { active: false } } as never),
      permissions: record("permissions", ADMIN_PERMISSIONS),
    });
    await settled();
    for (const name of ["status", "version", "dashboard", "permissions"]) {
      expect(seen[name], name).toBeDefined();
      expect(seen[name][0], name).toBeInstanceOf(AbortSignal);
    }
  });

  it("abort a pending app-wide read when the app unmounts", async () => {
    const status = heldReads<StatusResponse>();
    const { unmount, settled } = renderWithApp(<Probe />, { status: status.read });
    await waitFor(() => expect(status.calls).toHaveLength(1));
    const signal = status.calls[0].signal;
    expect(signal).toBeInstanceOf(AbortSignal);
    expect(signal?.aborted).toBe(false);
    unmount();
    expect(signal?.aborted).toBe(true);
    await settled();
  });

  it("abort the older read when a newer one starts, and show the newer answer", async () => {
    const status = heldReads<StatusResponse>();
    const { settled } = renderWithApp(<Probe />, { status: status.read });
    await waitFor(() => expect(status.calls).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "refresh" }));
    expect(status.calls).toHaveLength(2);
    expect(status.calls[0].signal?.aborted).toBe(true);
    expect(status.calls[1].signal?.aborted).toBe(false);
    await act(async () => status.calls[1].resolve(nightlyStatus({ cluster: { server: "newer", version: "4.22.8", user: "me" } })));
    await settled();
    expect(screen.getByTestId("server").textContent).toBe("newer");
  });
});
