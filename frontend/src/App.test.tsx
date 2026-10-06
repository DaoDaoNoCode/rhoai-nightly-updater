import React from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { DashboardDevRoute } from "./App";
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
