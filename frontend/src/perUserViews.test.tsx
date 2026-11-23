import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import { App } from "./App";
import { jsonResponse, stubApi } from "./test/apiStub";
import { nightlyStatus } from "./test/providers";
import { ADMIN_ONLY_GETS, USER_ALLOWED_GETS, stubUserApi } from "./test/userFixtures";
import { deferred } from "./test/utils";

// Per-user views (docs/research/per-user-views.md), through the whole app:
// the navigation and the pages per role, and that a user's tab never asks
// for a read only cluster-admins may make.

function openApp(path: string) {
  window.history.pushState({}, "", path);
  return render(<App />);
}

/** Every GET the tab made, as "/path?query". */
const gets = (calls: string[]) => calls.filter((c) => c.startsWith("GET ")).map((c) => c.slice(4));

// The sidebar is collapsed at jsdom's width, so its links are in the DOM but hidden.
const navLabels = () => within(screen.getByRole("navigation", { name: "Global", hidden: true })).getAllByRole("link", { hidden: true }).map((l) => l.textContent);

/** Let pending reads and their state updates settle. */
async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => { await new Promise((r) => setTimeout(r, 0)); });
}

beforeEach(() => {
  window.history.pushState({}, "", "/");
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("navigation per role", () => {
  it("a cluster-admin sees every page, as before", async () => {
    stubApi({ "/api/status": nightlyStatus(), "/api/operation": { inProgress: false, operation: null }, "/api/version": { version: "dev", commit: "c", buildDate: "d", templateOutdated: false } });
    openApp("/builds");
    await waitFor(() => expect(navLabels()).toEqual(["Status", "Components", "Build Explorer", "Dashboard Dev", "Environment", "Recipes", "Diagnostics"]));
  });

  it("a user sees Status, Build Explorer and Environment", async () => {
    stubUserApi();
    openApp("/builds");
    await waitFor(() => expect(navLabels()).toEqual(["Status", "Build Explorer", "Environment"]));
  });

  it("an answer without a role is a user's (fail closed)", async () => {
    stubUserApi({ "/api/user/permissions": { canMutate: false, user: "dev-user" } });
    openApp("/builds");
    await waitFor(() => expect(navLabels()).toEqual(["Status", "Build Explorer", "Environment"]));
  });

  it("shows no cluster-admin page or link while the role is not known", async () => {
    const permissions = deferred<Response>();
    const api = stubUserApi({ "/api/user/permissions": () => permissions.promise });
    openApp("/components");
    await settle();
    expect(navLabels()).toEqual(["Status", "Build Explorer", "Environment"]);
    expect(screen.getByRole("heading", { name: "Loading page" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Components" })).not.toBeInTheDocument();
    expect(gets(api.calls).filter((c) => ADMIN_ONLY_GETS.some((re) => re.test(c)))).toEqual([]);
    permissions.resolve(jsonResponse({ canMutate: false, user: "dev-user", role: "user" }));
    expect(await screen.findByRole("heading", { name: "Only cluster-admins can see this page" })).toBeInTheDocument();
  });

  it.each([["/components"], ["/dashboard-dev"], ["/recipes"], ["/diagnostics"]])("a direct link to %s shows that only cluster-admins can see it", async (path) => {
    const api = stubUserApi();
    openApp(path);
    expect(await screen.findByRole("heading", { name: "Only cluster-admins can see this page" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Go to Status" })).toBeInTheDocument();
    await settle();
    expect(gets(api.calls).filter((c) => ADMIN_ONLY_GETS.some((re) => re.test(c)))).toEqual([]);
  });

  it("the user menu names the user from the permission check", async () => {
    stubUserApi();
    openApp("/");
    expect(await screen.findByRole("button", { name: "User menu for dev-user" })).toBeInTheDocument();
  });
});

describe("a user's tab makes no read only cluster-admins may make", () => {
  it.each([
    ["Status", "/", "What runs on this cluster"],
    ["Build Explorer", "/builds", "Nightly builds on Quay"],
    ["Environment", "/test-resources", "What runs in your data science projects"],
    ["Environment, another project", "/test-resources?project=llm-eval", "What runs in your data science projects"],
    ["Not found", "/nowhere", "404"],
  ])("%s", async (_name, path, text) => {
    const api = stubUserApi();
    openApp(path);
    expect(await screen.findByText(text, { exact: false })).toBeInTheDocument();
    if (path.startsWith("/test-resources")) {
      // Every part of the project has been read.
      await screen.findByRole("grid", { name: /^MLflow sample data in / });
      await screen.findByText("Download sample-artifact-check.yaml");
    }
    await settle();
    const made = gets(api.calls);
    expect(made.filter((c) => ADMIN_ONLY_GETS.some((re) => re.test(c)))).toEqual([]);
    expect(made.filter((c) => !USER_ALLOWED_GETS.some((re) => re.test(c)))).toEqual([]);
    // Not even a refused one: no request was answered 403 admin_only.
    expect(await Promise.all(api.fetch.mock.results.map(async (r) => (await r.value as Response).status))).not.toContain(403);
  });

  it("keeps the release check, which every user may make", async () => {
    const api = stubUserApi({ "/api/version": { version: "v5.0.0", commit: "c", buildDate: "d", templateOutdated: false } });
    openApp("/");
    await screen.findByText("What runs on this cluster", { exact: false });
    await waitFor(() => expect(gets(api.calls)).toContain("/api/update-check"));
  });
});
