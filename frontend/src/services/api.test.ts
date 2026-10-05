import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  errorMessage,
  getStatus,
  isApiError,
  parseErrorResponse,
  repairDSC,
  streamSSE,
  toApiError,
  type StreamDetachReason,
} from "./api";
import type { UpdateStep } from "../types";
import { jsonResponse, sseEvent, sseResponse } from "../test/utils";

afterEach(() => {
  vi.useRealTimers();
});

describe("parseErrorResponse", () => {
  it.each([
    { name: "409 cluster busy", status: 409, body: { error: "Another cluster operation is in progress. Please wait.", errorCode: "cluster_busy" }, code: "cluster_busy", message: "Another cluster operation is in progress. Please wait." },
    { name: "403 read-only", status: 403, body: { error: "Read-only access", errorCode: "forbidden" }, code: "forbidden", message: "Read-only access" },
    { name: "429 rate limited", status: 429, body: { error: "Too many requests", errorCode: "rate_limited" }, code: "rate_limited", message: "Too many requests" },
    { name: "503 shutting down", status: 503, body: { error: "The updater is restarting.", errorCode: "shutting_down" }, code: "shutting_down", message: "The updater is restarting." },
    { name: "body without errorCode", status: 404, body: { error: "nope" }, code: "not_found", message: "nope" },
    { name: "message field", status: 500, body: { message: "boom" }, code: "internal", message: "boom" },
  ])("parses the JSON body: $name", async ({ status, body, code, message }) => {
    const err = await parseErrorResponse(jsonResponse(body, status));
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(status);
    expect(err.errorCode).toBe(code);
    expect(err.message).toBe(message);
    expect(err.details).toEqual(body);
  });

  it("keeps plain-text bodies with the status prefix", async () => {
    const err = await parseErrorResponse(new Response("Invalid image", { status: 400, headers: { "Content-Type": "text/plain" } }));
    expect(err.message).toBe("400: Invalid image");
    expect(err.errorCode).toBe("bad_request");
  });

  it("does not dump HTML error pages", async () => {
    const err = await parseErrorResponse(new Response("<html><body>Bad gateway</body></html>", { status: 502, headers: { "Content-Type": "text/html" } }));
    expect(err.message).toBe("Request failed (HTTP 502)");
    expect(err.errorCode).toBe("internal");
  });
});

describe("request()", () => {
  it("throws an ApiError with the backend errorCode", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "operator version changed", errorCode: "validation" }, 422)));
    await expect(repairDSC("default-dsc", "reset-defaults", "3.6.0")).rejects.toMatchObject({
      name: "ApiError", status: 422, errorCode: "validation", message: "operator version changed",
    });
  });

  it("reports an HTML login page as an expired session", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("<html>Login</html>", { status: 200, headers: { "Content-Type": "text/html" } })));
    await expect(getStatus()).rejects.toMatchObject({ errorCode: "session_expired", status: 200 });
  });

  it("maps a network failure to errorCode network", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("Failed to fetch"); }));
    await expect(getStatus()).rejects.toMatchObject({ errorCode: "network", status: 0 });
  });

  it("does not classify a digest that contains 403 as a permission error", async () => {
    // A06-9: a copy/paste typo in a digest produced "You do not have permission".
    const body = { error: "invalid image reference: quay.io/rhoai/rhoai-fbc-fragment@sha256:403abc", errorCode: "validation" };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(body, 400)));
    const err = await getStatus().catch((e: unknown) => e);
    expect(isApiError(err) && err.errorCode).toBe("validation");
    expect(isApiError(err) && err.status).toBe(400);
  });
});

describe("toApiError / errorMessage", () => {
  it("normalizes timeouts, aborts, strings and unknown values", () => {
    expect(toApiError(new DOMException("t", "TimeoutError")).errorCode).toBe("timeout");
    expect(toApiError(new DOMException("a", "AbortError")).errorCode).toBe("aborted");
    expect(toApiError("plain").message).toBe("plain");
    expect(errorMessage(undefined, "fallback")).toBe("fallback");
    const original = new ApiError({ status: 409, errorCode: "cluster_busy", message: "busy" });
    expect(toApiError(original)).toBe(original);
  });
});

type Outcome =
  | { kind: "done"; success: boolean; error?: string; apiError?: ApiError }
  | { kind: "drop"; reason: StreamDetachReason };

function openStream(opts: { idleTimeoutMs?: number } = {}) {
  const steps: UpdateStep[] = [];
  const outcomes: Outcome[] = [];
  const controller = streamSSE(
    "/api/test/stream",
    { image: "x" },
    (s) => steps.push(s),
    (success, error, apiError) => outcomes.push({ kind: "done", success, error, apiError }),
    (reason) => outcomes.push({ kind: "drop", reason }),
    opts,
  );
  return { controller, steps, outcomes };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

describe("streamSSE lifecycle", () => {
  it("reports success only after operation_complete", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    const { steps, outcomes } = openStream();
    sse.push(sseEvent("validate_prerequisites", "success", "ok"));
    sse.push(sseEvent("operation_complete", "success", "done"));
    sse.close();
    await vi.waitFor(() => expect(outcomes).toHaveLength(1));
    expect(outcomes[0]).toMatchObject({ kind: "done", success: true });
    expect(steps.map((s) => s.step)).toEqual(["validate_prerequisites", "operation_complete"]);
  });

  it("calls the end callback exactly once when the caller aborts mid-stream (A06-1)", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
      init.signal?.addEventListener("abort", () => sse.fail());
      return sse.response;
    }));
    const { controller, steps, outcomes } = openStream();
    sse.push(sseEvent("validate_prerequisites", "success", "ok"));
    await vi.waitFor(() => expect(steps).toHaveLength(1));
    controller.abort();
    await flush();
    await flush();
    expect(outcomes).toEqual([{ kind: "drop", reason: "aborted" }]);
  });

  it("reports aborted when aborted before the response arrives", async () => {
    vi.stubGlobal("fetch", vi.fn((_url: string, init: RequestInit) => new Promise((_res, rej) => {
      init.signal?.addEventListener("abort", () => rej(new DOMException("aborted", "AbortError")));
    })));
    const { controller, outcomes } = openStream();
    controller.abort();
    await flush();
    expect(outcomes).toEqual([{ kind: "drop", reason: "aborted" }]);
  });

  it("reports a dropped connection and a body that ends without a result", async () => {
    const dropped = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => dropped.response));
    const a = openStream();
    dropped.push(sseEvent("delete_csv", "running"));
    dropped.fail();
    await vi.waitFor(() => expect(a.outcomes).toEqual([{ kind: "drop", reason: "connection_lost" }]));

    const ended = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => ended.response));
    const b = openStream();
    ended.push(sseEvent("delete_csv", "success"));
    ended.close();
    await vi.waitFor(() => expect(b.outcomes).toEqual([{ kind: "drop", reason: "ended_without_result" }]));
  });

  it("parses JSON error bodies and exposes the errorCode (A06-6)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Another cluster operation is in progress. Please wait.", errorCode: "cluster_busy" }, 409)));
    const { outcomes } = openStream();
    await vi.waitFor(() => expect(outcomes).toHaveLength(1));
    const outcome = outcomes[0];
    expect(outcome.kind).toBe("done");
    if (outcome.kind !== "done") return;
    expect(outcome.success).toBe(false);
    expect(outcome.error).toBe("Another cluster operation is in progress. Please wait.");
    expect(outcome.apiError).toMatchObject({ status: 409, errorCode: "cluster_busy" });
  });

  it("passes the errorCode of a failed operation_complete", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    const { outcomes } = openStream();
    sse.push(sseEvent("operation_complete", "failed", "Catalog validation failed", { errorCode: "catalog_invalid" }));
    sse.close();
    await vi.waitFor(() => expect(outcomes).toHaveLength(1));
    expect(outcomes[0]).toMatchObject({ kind: "done", success: false, error: "Catalog validation failed", apiError: { errorCode: "catalog_invalid" } });
  });

  it("detaches with reason stalled when nothing arrives within the idle timeout", async () => {
    vi.useFakeTimers();
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
      init.signal?.addEventListener("abort", () => sse.fail());
      return sse.response;
    }));
    const { outcomes } = openStream({ idleTimeoutMs: 1000 });
    await vi.advanceTimersByTimeAsync(500);
    sse.push(": heartbeat\n\n");
    await vi.advanceTimersByTimeAsync(900);
    expect(outcomes).toEqual([]); // the heartbeat re-armed the watchdog
    await vi.advanceTimersByTimeAsync(200);
    expect(outcomes).toEqual([{ kind: "drop", reason: "stalled" }]);
  });

  it("treats a missing response as a failure, not a running operation", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("Failed to fetch"); }));
    const { outcomes } = openStream();
    await vi.waitFor(() => expect(outcomes).toHaveLength(1));
    expect(outcomes[0]).toMatchObject({ kind: "done", success: false, apiError: { errorCode: "network" } });
  });
});
