import { describe, expect, it } from "vitest";
import {
  COLOR_SCHEMES,
  COLOR_SCHEME_KEY,
  CONTRAST_MODES,
  CONTRAST_MODE_KEY,
  LEGACY_THEME_KEY,
  applyThemeClasses,
  migrateLegacyTheme,
  readStoredTheme,
  resolveTheme,
} from "./themeCore";
import { ALL_SYSTEMS } from "./themeTestUtils";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (k: string) => (data.has(k) ? data.get(k)! : null),
    setItem: (k: string, v: string) => void data.set(k, v),
    removeItem: (k: string) => void data.delete(k),
  };
}

describe("resolveTheme", () => {
  it.each(ALL_SYSTEMS)("System follows the OS: %j", (system) => {
    const got = resolveTheme("system", "system", system);
    expect(got.dark).toBe(system.dark);
    // High contrast wins; reduced transparency gives the solid default; else glass.
    expect(got.contrast).toBe(system.moreContrast ? "high-contrast" : system.reducedTransparency ? "default" : "glass");
  });

  it.each(ALL_SYSTEMS)("an explicit choice wins over the OS: %j", (system) => {
    expect(resolveTheme("light", "default", system)).toEqual({ dark: false, contrast: "default" });
    expect(resolveTheme("dark", "glass", system)).toEqual({ dark: true, contrast: "glass" });
    expect(resolveTheme("light", "high-contrast", system)).toEqual({ dark: false, contrast: "high-contrast" });
  });

  it("never combines glass and high contrast", () => {
    for (const scheme of COLOR_SCHEMES) {
      for (const mode of CONTRAST_MODES) {
        for (const system of ALL_SYSTEMS) {
          const root = document.createElement("html");
          applyThemeClasses(root, resolveTheme(scheme, mode, system));
          expect(root.classList.contains("pf-v6-theme-felt")).toBe(true);
          expect(root.classList.contains("pf-v6-theme-glass") && root.classList.contains("pf-v6-theme-high-contrast")).toBe(false);
        }
      }
    }
  });
});

describe("storage", () => {
  it.each([
    ["dark", "dark"],
    ["light", "light"],
    ["sepia", "system"],
  ])("migrates the pre-3.0.0 pf-theme=%s to color scheme %s and removes the old key", (legacy, scheme) => {
    const s = memoryStorage({ [LEGACY_THEME_KEY]: legacy });
    expect(readStoredTheme(s)).toEqual({ scheme, contrast: "system" });
    expect(s.data.has(LEGACY_THEME_KEY)).toBe(false);
    expect(s.data.get(COLOR_SCHEME_KEY)).toBe(scheme === "system" ? undefined : scheme);
  });

  it("keeps a stored color scheme over the legacy key", () => {
    const s = memoryStorage({ [LEGACY_THEME_KEY]: "dark", [COLOR_SCHEME_KEY]: "light" });
    migrateLegacyTheme(s);
    expect(s.data.get(COLOR_SCHEME_KEY)).toBe("light");
    expect(s.data.has(LEGACY_THEME_KEY)).toBe(false);
  });

  it("reads both choices, and System for unknown values or no storage", () => {
    expect(readStoredTheme(memoryStorage({ [COLOR_SCHEME_KEY]: "dark", [CONTRAST_MODE_KEY]: "high-contrast" }))).toEqual({ scheme: "dark", contrast: "high-contrast" });
    expect(readStoredTheme(memoryStorage({ [COLOR_SCHEME_KEY]: "blue", [CONTRAST_MODE_KEY]: "loud" }))).toEqual({ scheme: "system", contrast: "system" });
    expect(readStoredTheme(null)).toEqual({ scheme: "system", contrast: "system" });
  });

  it("survives a storage that throws", () => {
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); }, removeItem: () => {} };
    expect(readStoredTheme(broken)).toEqual({ scheme: "system", contrast: "system" });
  });
});
