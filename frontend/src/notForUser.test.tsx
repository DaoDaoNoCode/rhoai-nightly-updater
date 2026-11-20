import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiError } from "./services/api";
import { describeError, isNotForUser } from "./errors";
import { describeLoadError } from "./outcomes";
import { PageErrorState } from "./components/PageStates";
import { LoadErrorAlert } from "./components/LoadErrorAlert";

// Per-user views: a 403 that means "not yours to see" is never shown as a
// failure ("Access denied", Retry, Diagnostics, a template hint).

const forbiddenForUser = new ApiError({ status: 403, errorCode: "forbidden_for_user", message: "pipelineservers is forbidden: User \"dev-user\" cannot list" });
const adminOnly = new ApiError({ status: 403, errorCode: "admin_only", message: "Only cluster-admins can read this. The deployment template is out of date: run make upgrade." });

describe("403s that mean 'not for you'", () => {
  it("are told apart from other errors", () => {
    expect(isNotForUser(forbiddenForUser)).toBe(true);
    expect(isNotForUser(adminOnly)).toBe(true);
    expect(isNotForUser(new ApiError({ status: 403, errorCode: "forbidden", message: "x" }))).toBe(false);
    expect(isNotForUser(new Error("x"))).toBe(false);
  });

  it.each([
    [forbiddenForUser, "You cannot see this", /no access to it/],
    [adminOnly, "Only cluster-admins can see this", /A cluster-admin sets this up/],
  ])("%s has its own words, never Access denied or a make upgrade hint", (err, title, body) => {
    for (const d of [describeError(err, "Could not load"), describeLoadError(err, "Could not load")]) {
      expect(d.title).toBe(title);
      expect(d.body).toMatch(body);
      expect(d.body).not.toMatch(/make upgrade|Access denied/);
      expect(d.variant).toBe("warning");
    }
  });

  it("a page that has nothing else to show says so, without Retry or Diagnostics", () => {
    render(<MemoryRouter><PageErrorState error={forbiddenForUser} title="Can't load your projects" onRetry={vi.fn()} /></MemoryRouter>);
    expect(screen.getByRole("heading", { name: "You cannot see this" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open Diagnostics" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Access denied/)).not.toBeInTheDocument();
  });

  it("a load alert has no Retry for it", () => {
    render(<LoadErrorAlert error={adminOnly} genericTitle="Could not load" onRetry={vi.fn()} />);
    expect(screen.getByText("Only cluster-admins can see this")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  });

  it("other errors keep Retry and Diagnostics", () => {
    render(<MemoryRouter><PageErrorState error={new ApiError({ status: 503, errorCode: "cluster_unavailable", message: "down" })} title="Can't load" onRetry={vi.fn()} /></MemoryRouter>);
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open Diagnostics" })).toBeInTheDocument();
  });
});
