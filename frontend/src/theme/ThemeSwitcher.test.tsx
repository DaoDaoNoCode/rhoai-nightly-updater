import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "./ThemeContext";
import { Dropdown, MenuToggle } from "@patternfly/react-core";
import { ThemeMenuGroups, ThemeSwitcher } from "./ThemeSwitcher";
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

describe("ThemeMenuGroups", () => {
  it("offers both choices as menu groups with the chosen ones checked, and applies a choice at once", () => {
    liveSystem({});
    render(
      <ThemeProvider>
        <Dropdown isOpen toggle={(ref) => <MenuToggle ref={ref}>Menu</MenuToggle>}>
          <ThemeMenuGroups />
        </Dropdown>
      </ThemeProvider>,
    );
    expect(screen.getByRole("heading", { name: "Color scheme" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Contrast mode" })).toBeInTheDocument();
    // Both start on System: one checked item per group.
    expect(screen.getAllByRole("menuitem", { name: /, selected$/ }).map((el) => el.getAttribute("aria-label"))).toEqual(["System, selected", "System, selected"]);

    // PatternFly's own selected state (check mark, pf-m-selected) on the
    // chosen item only; the accessible name keeps the visible text.
    const isChecked = (item: HTMLElement) => item.classList.contains("pf-m-selected") && !!item.querySelector(".pf-v6-c-menu__item-select-icon");
    const checked = (name: string) => isChecked(screen.getByRole("menuitem", { name }));
    expect(screen.getAllByRole("menuitem", { name: /, selected$/ }).every(isChecked)).toBe(true);
    expect(screen.getAllByRole("menuitem").filter(isChecked)).toHaveLength(2);
    expect(screen.getByRole("menuitem", { name: "Dark" })).toHaveTextContent("Dark");
    expect(checked("Dark")).toBe(false);

    fireEvent.click(screen.getByRole("menuitem", { name: "Dark" }));
    expect(html().contains("pf-v6-theme-dark")).toBe(true);
    expect(localStorage.getItem(COLOR_SCHEME_KEY)).toBe("dark");
    expect(screen.getByRole("menuitem", { name: "Dark, selected" })).toHaveTextContent("Dark");
    expect(checked("Dark, selected")).toBe(true);
    expect(checked("Light")).toBe(false);

    fireEvent.click(screen.getByRole("menuitem", { name: "High contrast" }));
    expect(html().contains("pf-v6-theme-high-contrast")).toBe(true);
    expect(localStorage.getItem(CONTRAST_MODE_KEY)).toBe("high-contrast");
    expect(screen.getByRole("menuitem", { name: "High contrast, selected" })).toBeInTheDocument();
  });
});
