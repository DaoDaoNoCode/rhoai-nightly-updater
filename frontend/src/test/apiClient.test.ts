import { describe, expect, it, vi } from "vitest";
import { deployDashboardLatestMain, deployDashboardPR, getBuildExplorerTags, repairDSC, revertDashboard, streamSSE } from "../services/api";
import { jsonBody, jsonResponse } from "./utils";

describe("Build Explorer tags", () => {
  it("requests versions without waiting for build dates", async () => {
    const paths: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      paths.push(url);
      return jsonResponse({ tags: [] });
    }));
    await getBuildExplorerTags();
    await getBuildExplorerTags(true);
    expect(paths).toEqual([
      "/api/build-explorer/tags?includeDates=false",
      "/api/build-explorer/tags?includeDates=true",
    ]);
  });
});

describe("Dashboard Dev requests", () => {
  it("sends main, PR and revert requests and retains partial-failure details", async () => {
    const calls: Array<{ url: string; method?: string; body?: BodyInit | null; contentType?: string }> = [];
    const partial = { success: false, message: "notebooks-ui failed; revert to resume operator", errorCode: "partial_failure", logs: ["operator paused"] };
    vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
      const headers = init.headers as Record<string, string> | undefined;
      calls.push({ url, method: init.method, body: init.body, contentType: headers?.["Content-Type"] });
      return jsonResponse(partial, 422);
    }));
    for (const result of [await deployDashboardLatestMain(), await deployDashboardPR(123), await revertDashboard()]) {
      expect(result).toEqual(partial);
    }
    expect(calls.map((c) => c.url)).toEqual(["/api/dashboard/deploy-main", "/api/dashboard/deploy-pr", "/api/dashboard/revert"]);
    expect(calls.every((c) => c.method === "POST")).toBe(true);
    // Body-less POSTs must still declare JSON: the backend rejects other types.
    expect(calls.map((c) => c.contentType)).toEqual(["application/json", "application/json", "application/json"]);
    expect(jsonBody(calls[1])).toEqual({ pr: 123 });
  });
});

describe("DSC repair", () => {
  it.each(["reset-defaults", "remove-extra-components", "remove-invalid"] as const)("rejects a validation error with the backend explanation (%s)", async (mode) => {
    const message = "operator version changed since the defaults preview; refresh and review defaults";
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: message, errorCode: "validation" }, 422)));
    // ApiError carries the status and code separately from the message.
    await expect(repairDSC("default-dsc", mode, "datasciencecluster.opendatahub.io/v2", "3.6.0")).rejects.toMatchObject({
      message, status: 422, errorCode: "validation",
    });
  });

  it("preserves a successful operation result", async () => {
    const result = { success: true, message: "DSC repaired", logs: [] };
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse(result)));
    await expect(repairDSC("default-dsc", "remove-invalid", undefined)).resolves.toEqual(result);
  });
});

function event(step: string, status: string, message: string): string {
  return "data: " + JSON.stringify({ step, status, message }) + "\n\n";
}

describe("stream results", () => {
  it.each([
    { name: "forbidden (backend JSON)", status: 403, contentType: "application/json", text: JSON.stringify({ error: "Read-only access", errorCode: "forbidden" }), success: false, error: /Read-only/ },
    { name: "oauth-proxy sign-in page (403 text/html)", status: 403, contentType: "text/html", text: "<html>Log In</html>", success: false, error: /session has expired/i },
    { name: "busy", status: 409, text: "Another operation is in progress", success: false, error: /409/ },
    { name: "validation", status: 400, text: "Invalid image", success: false, error: /400.*Invalid image/ },
    { name: "truncated stream", text: event("delete_subscription", "success", "Deleted"), drop: true },
    { name: "empty stream", text: "", drop: true },
    { name: "pending InstallPlan", text: event("verify_installplan", "skipped", "Pending") + event("operation_complete", "success", "Installation initiated"), success: true },
    { name: "terminal failure", text: event("operation_complete", "failed", "Catalog validation failed"), success: false, error: /Catalog validation failed/ },
    { name: "session expired", contentType: "text/html", text: "Login", success: false, error: /session has expired/i },
  ])("distinguish rejection, disconnect, pending and terminal failure: $name", async (tc) => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(tc.text, { status: tc.status ?? 200, headers: { "Content-Type": tc.contentType ?? "text/event-stream" } })));
    const actual = await new Promise<{ success?: boolean; error?: string; drop?: true }>((resolve) => {
      streamSSE("/api/test", {}, () => {}, (success, error) => resolve({ success, error }), () => resolve({ drop: true }));
    });
    if (tc.drop) {
      expect(actual.drop).toBe(true);
      return;
    }
    expect(actual.drop).toBeUndefined();
    expect(actual.success).toBe(tc.success);
    if (tc.error) expect(actual.error).toMatch(tc.error);
  });
});
