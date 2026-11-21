import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { getMaaS, onSessionExpired } from "./services/api";
import { useMaaSStatus } from "./components/environment/MaaSTab";
import { useSessionExpired } from "./state/Permissions";
import { renderWithApp, settleApps } from "./test/providers";
import { jsonResponse } from "./test/utils";

// A read cancelled on purpose (useLatestRequest: a newer read started, or
// the page went away) is not a sign that the session expired.
//
// fetch() aborted after the headers arrived errors the response body with
// the signal's abort reason (Fetch standard, "abort the fetch() call": "If
// response's body is non-null and is readable, then error response's body
// with error"), so reading an error body then rejects with an AbortError.

/** A response whose headers arrived and whose body is still coming, cut off when `signal` aborts, as fetch() does. */
function bodyStillArriving(status: number, signal: AbortSignal | null | undefined): Response {
  const body = new ReadableStream<Uint8Array>({
    start(ctrl) {
      signal?.addEventListener("abort", () => ctrl.error(signal.reason));
    },
  });
  return new Response(body, { status, headers: { "Content-Type": "application/json" } });
}

const MAAS = { platform: { deployed: true, ready: false }, parts: [{ name: "x", state: "waiting" }], subscription: "s" };

afterEach(() => {
  vi.useRealTimers();
});

describe("an aborted read", () => {
  it.each([401, 403])("with a %i whose body was still arriving is reported as aborted, never as an expired session", async (status) => {
    vi.stubGlobal("fetch", vi.fn(async (_url: string, init?: RequestInit) => bodyStillArriving(status, init?.signal)));
    const expired = vi.fn();
    const off = onSessionExpired(expired);
    try {
      const controller = new AbortController();
      const read = getMaaS(controller.signal).catch((e: unknown) => e);
      await new Promise((r) => setTimeout(r, 0));
      controller.abort();
      expect(await read).toMatchObject({ errorCode: "aborted" });
      expect(expired).not.toHaveBeenCalled();
    } finally {
      off();
    }
  });

  it("superseded by a newer read: no expired-session state, and polling goes on", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    let reads = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (!String(url).startsWith("/api/resources/maas")) return jsonResponse({}, 404);
      reads++;
      // The first read gets a 403 whose body is still arriving when the reload supersedes it.
      return reads === 1 ? bodyStillArriving(403, init?.signal) : jsonResponse(MAAS);
    }));
    let reload: () => void = () => {};
    const Probe = () => {
      const m = useMaaSStatus(0, false);
      reload = m.reload;
      return <p data-testid="probe">{`${useSessionExpired()}|${m.data?.subscription ?? "-"}`}</p>;
    };
    const app = renderWithApp(<Probe />);
    await app.settled();
    expect(reads).toBe(1);
    await act(async () => { reload(); });
    await app.settled();
    expect(screen.getByTestId("probe")).toHaveTextContent("false|s");
    const before = reads;
    for (let i = 0; i < 3; i++) await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(reads).toBeGreaterThan(before);
    expect(screen.getByTestId("probe")).toHaveTextContent("false|s");
    await settleApps();
  });
});
