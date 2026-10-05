import { vi } from "vitest";
import { jsonResponse } from "./utils";

export type Handler = (url: string, init: RequestInit | undefined) => Response | Promise<Response>;

export interface ApiStub {
  /** Every request as "METHOD /path?query". */
  calls: string[];
  /** Parsed JSON bodies of non-GET requests, by "METHOD /path". */
  bodies: Record<string, unknown[]>;
  fetch: ReturnType<typeof vi.fn>;
}

/**
 * Stub global fetch with handlers keyed by "METHOD /path" (query ignored) or
 * "/path" (any method). Unknown requests fail the test loudly with a 599.
 * `/api/user/permissions` defaults to canMutate:true, `/api/pageview` to 204.
 */
export function stubApi(handlers: Record<string, Handler | unknown>): ApiStub {
  const calls: string[] = [];
  const bodies: Record<string, unknown[]> = {};
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    const method = (init?.method ?? "GET").toUpperCase();
    const path = url.split("?")[0];
    calls.push(`${method} ${url}`);
    if (method !== "GET" && typeof init?.body === "string") {
      (bodies[`${method} ${path}`] ??= []).push(JSON.parse(init.body));
    } else if (method !== "GET") {
      (bodies[`${method} ${path}`] ??= []).push(undefined);
    }
    const handler = handlers[`${method} ${path}`] ?? handlers[path];
    if (handler === undefined) {
      if (path === "/api/user/permissions") return jsonResponse({ canMutate: true, user: "tester" });
      if (path === "/api/pageview") return new Response(null, { status: 204 });
      return jsonResponse({ error: `unexpected request ${method} ${url}`, errorCode: "test_unexpected" }, 599);
    }
    if (typeof handler === "function") return (handler as Handler)(url, init);
    return jsonResponse(handler);
  });
  vi.stubGlobal("fetch", fetchMock);
  return { calls, bodies, fetch: fetchMock };
}

export { jsonResponse };
