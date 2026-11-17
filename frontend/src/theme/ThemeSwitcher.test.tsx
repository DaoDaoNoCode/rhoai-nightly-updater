import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "./ThemeContext";
import { ThemeSwitcher } from "./ThemeSwitcher";
import { COLOR_SCHEME_KEY, CONTRAST_MODE_KEY, LEGACY_THEME_KEY, MEDIA_QUERIES } from "./themeCore";

/** A matchMedia whose answers can change, firing "change" like a browser does. */
function liveSystem(initial: Record<string, boolean>) {
  const answers = { ...initial };
  const listeners = new Map<string, Set<() => void>>();
  vi.stubGlobal("matchMedia", (q: string) => ({
    get matches() { return !!answers[q]; },
    media: q,
    addEventListener: (_: string, fn: () => void) => { if (!listeners.has(q)) listeners.set(q, new Set()); listeners.get(q)!.add(fn); },
    removeEventListener: (_: string, fn: () => void) => listeners.get(q)?.delete(fn),
  }));
  return (q: string, value: boolean) => act(() => {
    answers[q] = value;
    listeners.get(q)?.forEach((fn) => fn());
  });
}

const html = () => document.documentElement.classList;

afterEach(() => {
  localStorage.clear();
  document.documentElement.className = "";
});

describe("ThemeSwitcher", () => {
  it("sets the color scheme and contrast mode, persists them, and never combines glass and high contrast", () => {
    liveSystem({});
    render(<ThemeProvider><ThemeSwitcher /></ThemeProvider>);
    // System, no OS preference: light and glass, felt always on.
    expect(html().contains("pf-v6-theme-felt")).toBe(true);
    expect(html().contains("pf-v6-theme-dark")).toBe(false);
    expect(html().contains("pf-v6-theme-glass")).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: /^Appearance: System color scheme, System contrast mode/ }));
    expect(screen.getByText("Following the system: Light, glass.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Dark" }));
    expect(html().contains("pf-v6-theme-dark")).toBe(true);
    expect(localStorage.getItem(COLOR_SCHEME_KEY)).toBe("dark");

    fireEvent.click(screen.getByRole("button", { name: "High contrast" }));
    expect(html().contains("pf-v6-theme-high-contrast")).toBe(true);
    expect(html().contains("pf-v6-theme-glass")).toBe(false);
    expect(localStorage.getItem(CONTRAST_MODE_KEY)).toBe("high-contrast");
    expect(screen.queryByText(/Following the system/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Glass" }));
    expect(html().contains("pf-v6-theme-glass")).toBe(true);
    expect(html().contains("pf-v6-theme-high-contrast")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Default" }));
    expect(html().contains("pf-v6-theme-glass") || html().contains("pf-v6-theme-high-contrast")).toBe(false);
    expect(screen.getByRole("button", { name: "Default" })).toHaveAttribute("aria-pressed", "true");
  });

  it("follows OS changes live while a choice is System", () => {
    const set = liveSystem({});
    render(<ThemeProvider><ThemeSwitcher /></ThemeProvider>);
    set(MEDIA_QUERIES.dark, true);
    expect(html().contains("pf-v6-theme-dark")).toBe(true);
    set(MEDIA_QUERIES.moreContrast, true);
    expect(html().contains("pf-v6-theme-high-contrast")).toBe(true);
    expect(html().contains("pf-v6-theme-glass")).toBe(false);
    set(MEDIA_QUERIES.moreContrast, false);
    set(MEDIA_QUERIES.reducedTransparency, true);
    expect(html().contains("pf-v6-theme-high-contrast") || html().contains("pf-v6-theme-glass")).toBe(false);
  });

  it("an explicit choice ignores the OS", () => {
    localStorage.setItem(COLOR_SCHEME_KEY, "light");
    localStorage.setItem(CONTRAST_MODE_KEY, "glass");
    const set = liveSystem({ [MEDIA_QUERIES.dark]: true });
    render(<ThemeProvider><ThemeSwitcher /></ThemeProvider>);
    set(MEDIA_QUERIES.moreContrast, true);
    expect(html().contains("pf-v6-theme-dark")).toBe(false);
    expect(html().contains("pf-v6-theme-glass")).toBe(true);
  });

  it("migrates the pre-3.0.0 dark choice", () => {
    localStorage.setItem(LEGACY_THEME_KEY, "dark");
    liveSystem({});
    render(<ThemeProvider><ThemeSwitcher /></ThemeProvider>);
    expect(html().contains("pf-v6-theme-dark")).toBe(true);
    expect(localStorage.getItem(LEGACY_THEME_KEY)).toBeNull();
    expect(screen.getByRole("button", { name: /^Appearance: Dark color scheme/ })).toBeInTheDocument();
  });
});
