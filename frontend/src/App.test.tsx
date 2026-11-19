import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { App, DashboardDevRoute, EnvironmentRoute } from "./App";
import { nightlyStatus } from "./test/providers";
import { NAV_ITEMS } from "./constants";

const Where: React.FC = () => <span data-testid="where">{useLocation().pathname + useLocation().search}</span>;

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/dashboard-dev" element={<DashboardDevRoute />} />
        <Route path="/environment" element={<EnvironmentRoute />} />
        <Route path="/test-resources" element={<p>Environment page</p>} />
      </Routes>
      <Where />
    </MemoryRouter>,
  );
}

describe("Environment route, /test-resources (A08-15)", () => {
  it("has its own nav item after Dashboard Dev and before Diagnostics", () => {
    expect(NAV_ITEMS.map((i) => i.label)).toEqual(["Status", "Components", "Build Explorer", "Dashboard Dev", "Environment", "Diagnostics"]);
    expect(NAV_ITEMS.find((i) => i.label === "Environment")?.path).toBe("/test-resources");
  });

  it("redirects the old /dashboard-dev?tab=resources link", async () => {
    renderAt("/dashboard-dev?tab=resources");
    expect(await screen.findByText("Environment page")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/test-resources");
  });

  it("/environment, the page's name, opens it with the same tab and project", async () => {
    renderAt("/environment?tab=models&project=dev");
    expect(await screen.findByText("Environment page")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/test-resources?tab=models&project=dev");
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("App shell", () => {
  it("replaces the page with one sign-in state when the session expired (A02-3, UX-Global-16)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "expired", errorCode: "session_expired" }), { status: 401, headers: { "Content-Type": "application/json" } })));
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Your session expired" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in again" })).toHaveAttribute("href", "/oauth/sign_in");
    // One message, not a banner over a page that repeats it.
    expect(screen.getAllByText("Your session expired")).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: "Status" })).not.toBeInTheDocument();
  });

  // The RHOAI dashboard link is the address the backend discovered
  // (status.dashboardURL), never a host guessed from the console's.
  it.each([
    ["the discovered dashboard URL", "https://rh-ai.apps.example.com", ["OpenShift console", "RHOAI dashboard"]],
    ["no dashboard link when its address is unknown", undefined, ["OpenShift console"]],
    ["no dashboard link for a non-https address", "http://rh-ai.apps.example.com", ["OpenShift console"]],
  ])("app launcher: %s", async (_name, dashboardURL, items) => {
    const status = nightlyStatus({ consoleURL: "https://console-openshift-console.apps.example.com", dashboardURL });
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const body = String(url).includes("/api/status") ? status
        : String(url).includes("/api/permissions") ? { canMutate: true, user: "me" }
        : {};
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Applications" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent?.trim())).toEqual(items);
    if (dashboardURL?.startsWith("https://")) {
      expect(within(menu).getByRole("menuitem", { name: /RHOAI dashboard/ })).toHaveAttribute("href", dashboardURL);
    }
  });

  // oauth-proxy's sign_out clears its session cookie; sign_in only showed
  // the sign-in page and left the session in place.
  it("logs out through oauth-proxy's sign_out", async () => {
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const body = String(url).includes("/api/status") ? nightlyStatus()
        : String(url).includes("/api/permissions") ? { canMutate: true, user: "me" }
        : {};
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: /^User menu for / }));
    expect(await screen.findByRole("menuitem", { name: "Log out" })).toHaveAttribute("href", "/oauth/sign_out");
  });
});
