import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type React from "react";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App } from "./App";
import { PageErrorBoundary, isChunkLoadError } from "./components/ErrorBoundary";
import { nightlyStatus } from "./test/providers";

// After the updater redeploys itself, the new image has new chunk names
// and the backend answers an old one with 404 (pkg/middleware/static.go).
// webpack's runtime then rejects the page's import() with a ChunkLoadError
// (webpack 5 lib/web/JsonpChunkLoadingRuntimeModule.js: name
// "ChunkLoadError", message "Loading chunk <id> failed.\n(<type>: <src>)").
function chunkLoadError(): Error {
  const src = "http://localhost/js/300.0123abcd.js";
  return Object.assign(new Error(`Loading chunk 300 failed.\n(missing: ${src})`), { name: "ChunkLoadError", type: "missing", request: src });
}

// The Components page's chunk is gone; Build Explorer's page has a bug.
vi.mock("./pages/ComponentsPage", () => ({
  get ComponentsPage() {
    throw chunkLoadError();
  },
}));
vi.mock("./pages/BuildExplorerPage", () => ({
  BuildExplorerPage: () => {
    throw new Error("a bug in the page");
  },
}));
// Diagnostics' chunk loads when the test says so.
const diagnosticsChunk = vi.hoisted(() => {
  let open: () => void = () => {};
  const loaded = new Promise<void>((resolve) => { open = resolve; });
  return { loaded, open: () => open() };
});
vi.mock("./pages/TroubleshootingPage", async () => {
  await diagnosticsChunk.loaded;
  return { TroubleshootingPage: () => <h1>Diagnostics page</h1> };
});

function openApp(path: string) {
  window.history.pushState({}, "", path);
  return render(<App />);
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    const body = String(url).includes("/api/status") ? nightlyStatus()
      : String(url).includes("/api/user/permissions") ? { canMutate: true, user: "me", role: "admin" }
      : String(url).includes("/api/operation") ? { inProgress: false, operation: null }
      : {};
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  }));
});
afterEach(() => {
  window.history.pushState({}, "", "/");
});

describe("a page whose files are gone (the updater was updated while the tab was open)", () => {
  it("keeps the masthead and navigation, and offers Reload", async () => {
    // React reports the error the boundary caught.
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    openApp("/components");
    const alert = await screen.findByRole("heading", { name: /This page could not be loaded/ });
    expect(alert).toBeInTheDocument();
    expect(screen.getByText(/A new version of the updater may have been installed since this tab was opened/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
    // The shell stays: the masthead's home link and the navigation.
    expect(screen.getByRole("link", { name: "RHOAI Nightly Updater home" })).toBeInTheDocument();
    expect(within(screen.getByRole("navigation", { name: "Global", hidden: true })).getAllByRole("link", { hidden: true }).length).toBeGreaterThan(0);
    expect(screen.queryByText("Something went wrong")).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: /^User menu for / })).toBeInTheDocument());
    expect(errors.mock.calls.some((args) => args.some((a) => a instanceof Error && a.name === "ChunkLoadError"))).toBe(true);
  });
});

describe("the page boundary", () => {
  const Throws: React.FC<{ error: Error }> = ({ error }) => {
    throw error;
  };

  it("reloads the whole page from Reload", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const onReload = vi.fn();
    render(<PageErrorBoundary onReload={onReload}><Throws error={chunkLoadError()} /></PageErrorBoundary>);
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it("recognizes webpack's JS and CSS chunk errors, and nothing else", () => {
    expect(isChunkLoadError(chunkLoadError())).toBe(true);
    // mini-css-extract-plugin's CSS chunk error.
    expect(isChunkLoadError(Object.assign(new Error("Loading CSS chunk 300 failed.\n(error: /css/300.0123abcd.css)"), { name: "ChunkLoadError", code: "CSS_CHUNK_LOAD_FAILED" }))).toBe(true);
    expect(isChunkLoadError(new Error("Loading chunk 300 failed."))).toBe(false);
    expect(isChunkLoadError(new TypeError("x is undefined"))).toBe(false);
    expect(isChunkLoadError({ name: "ChunkLoadError" })).toBe(false);
  });

  it("is cleared by going to another page, which loads", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    openApp("/components");
    expect(await screen.findByRole("heading", { name: /This page could not be loaded/ })).toBeInTheDocument();
    fireEvent.click(within(screen.getByRole("navigation", { name: "Global", hidden: true })).getByRole("link", { name: "Status", hidden: true }));
    await waitFor(() => expect(screen.queryByRole("heading", { name: /This page could not be loaded/ })).not.toBeInTheDocument());
    expect(window.location.pathname).toBe("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Status" })).toBeInTheDocument();
  });
});

describe("moving between pages (unchanged)", () => {
  // react-router 7 BrowserRouter navigates in a transition, so React keeps
  // the page shown until the next page's chunk is loaded, without the
  // "Loading page" fallback in between.
  it("keeps the current page shown while the next page's chunk loads", async () => {
    openApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Status" })).toBeInTheDocument();
    fireEvent.click(within(screen.getByRole("navigation", { name: "Global", hidden: true })).getByRole("link", { name: "Diagnostics", hidden: true }));
    await act(async () => {});
    expect(screen.getByRole("heading", { level: 1, name: "Status" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Loading page" })).not.toBeInTheDocument();
    diagnosticsChunk.open();
    expect(await screen.findByRole("heading", { name: "Diagnostics page" })).toBeInTheDocument();
  });
});

describe("any other error on a page", () => {
  it("keeps the app-wide error screen, as before", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    openApp("/builds");
    expect(await screen.findByText("Something went wrong")).toBeInTheDocument();
    expect(screen.getByText("a bug in the page")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload page" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "RHOAI Nightly Updater home" })).not.toBeInTheDocument();
    expect(errors).toHaveBeenCalled();
  });
});
