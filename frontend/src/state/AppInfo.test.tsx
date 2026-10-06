import React from "react";
import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { overrideFromDashboardResponse, useDashboardOverride, usePermissions, useSessionExpired } from "./AppInfo";
import { ApiError, getComponents, getUserPermissions, isSessionExpired } from "../services/api";
import { renderWithApp } from "../test/providers";
import { jsonResponse } from "../test/utils";

const Probe: React.FC = () => {
  const p = usePermissions();
  const expired = useSessionExpired();
  const { override } = useDashboardOverride();
  return <pre data-testid="probe">{JSON.stringify({ status: p.status, canMutate: p.canMutate, reason: p.reason, expired, override: override?.active ?? null })}</pre>;
};
const probe = () => JSON.parse(screen.getByTestId("probe").textContent || "{}");

describe("permissions (A02-3)", () => {
  it.each([
    ["allowed", async () => ({ canMutate: true, user: "me" }), { status: "allowed", canMutate: true, reason: null }],
    ["denied by the backend", async () => ({ canMutate: false, user: "me" }), { status: "denied", canMutate: false }],
    ["403", async () => { throw new ApiError({ status: 403, errorCode: "forbidden", message: "Read-only access" }); }, { status: "denied", canMutate: false }],
    ["503 authorization_unavailable is never 'allowed'", async () => { throw new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot verify" }); }, { status: "unknown", canMutate: false }],
    ["network error", async () => { throw new TypeError("Failed to fetch"); }, { status: "unknown", canMutate: false }],
  ])("%s", async (_name, permissions, expected) => {
    renderWithApp(<Probe />, { permissions });
    await waitFor(() => expect(probe()).toMatchObject(expected));
  });

  it("starts as 'checking' (disabled) until the first answer", () => {
    renderWithApp(<Probe />, { permissions: () => new Promise(() => {}) });
    expect(probe()).toMatchObject({ status: "checking", canMutate: false });
  });
});

describe("session expiry (A02-3)", () => {
  it("any API call answered with 401 session_expired raises the app-wide flag and disables changes", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Your OpenShift session has expired. Log in again.", errorCode: "session_expired" }, 401)));
    renderWithApp(<Probe />);
    await expect(getComponents()).rejects.toMatchObject({ status: 401, errorCode: "session_expired" });
    await waitFor(() => expect(probe()).toMatchObject({ expired: true, canMutate: false }));
    expect(probe().reason).toMatch(/session has expired/);
  });

  it("treats oauth-proxy's HTML sign-in page as an expired session, but not other 401s", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("<html>login</html>", { status: 200, headers: { "Content-Type": "text/html" } })));
    const err = await getComponents().catch((e) => e);
    expect(isSessionExpired(err)).toBe(true);
    expect(isSessionExpired(new ApiError({ status: 401, errorCode: "unauthorized", message: "SA token rejected" }))).toBe(false);
  });

  it("shows oauth-proxy's 403 HTML sign-in page as an expired session, not as read-only access (N3)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("<!DOCTYPE html><html>Log In</html>", { status: 403, headers: { "Content-Type": "text/html" } })));
    // The real permissions call, as after a night with the tab open.
    renderWithApp(<Probe />, { permissions: getUserPermissions });
    await waitFor(() => expect(probe()).toMatchObject({ expired: true, canMutate: false, status: "unknown" }));
    expect(probe().reason).toMatch(/session has expired/);
  });

  it("keeps a JSON 403 a permission answer", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "Read-only access", errorCode: "forbidden" }, 403)));
    renderWithApp(<Probe />, { permissions: getUserPermissions });
    await waitFor(() => expect(probe()).toMatchObject({ expired: false, status: "denied" }));
  });
});

describe("Dashboard Dev override (A04-1)", () => {
  it("reads the override from the state and from a 404 dashboard_not_deployed body", () => {
    expect(overrideFromDashboardResponse({ override: { active: true } } as never)?.active).toBe(true);
    const notDeployed = new ApiError({ status: 404, errorCode: "dashboard_not_deployed", message: "x", details: { override: { active: true, operatorPaused: true } } });
    expect(overrideFromDashboardResponse(notDeployed)?.active).toBe(true);
    // Other errors say nothing about the session: keep what was known.
    expect(overrideFromDashboardResponse(new ApiError({ status: 503, errorCode: "network", message: "x" }))).toBeUndefined();
  });

  it("exposes an active session to every page", async () => {
    renderWithApp(<Probe />, { dashboard: async () => ({ override: { active: true, operatorPaused: true, sessionRecorded: true, stale: false, dashboardDeleting: false } }) as never });
    await waitFor(() => expect(probe().override).toBe(true));
  });
});
