import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { App, DashboardDevRoute, rhoaiDashboardURL } from "./App";
import { NAV_ITEMS } from "./constants";

const Where: React.FC = () => <span data-testid="where">{useLocation().pathname + useLocation().search}</span>;

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/dashboard-dev" element={<DashboardDevRoute />} />
        <Route path="/test-resources" element={<p>Test resources page</p>} />
      </Routes>
      <Where />
    </MemoryRouter>,
  );
}

describe("Test resources route (A08-15)", () => {
  it("has its own nav item after Dashboard Dev and before Diagnostics", () => {
    expect(NAV_ITEMS.map((i) => i.label)).toEqual(["Status", "Components", "Build Explorer", "Dashboard Dev", "Test resources", "Diagnostics"]);
    expect(NAV_ITEMS.find((i) => i.label === "Test resources")?.path).toBe("/test-resources");
  });

  it("redirects the old /dashboard-dev?tab=resources link", async () => {
    renderAt("/dashboard-dev?tab=resources");
    expect(await screen.findByText("Test resources page")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/test-resources");
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

  it("links the RHOAI dashboard next to the console route", () => {
    expect(rhoaiDashboardURL("https://console-openshift-console.apps.example.com"))
      .toBe("https://data-science-gateway.apps.example.com");
  });
});
