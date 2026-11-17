import { describe, expect, it } from "vitest";
import { COLOR_SCHEMES, CONTRAST_MODES, resolveTheme, type SystemPreferences } from "./themeCore";
import { systemNotes } from "./themeLabels";
import { ALL_SYSTEMS } from "./themeTestUtils";

// A dark OS that asks for nothing else: System resolves to Dark and Glass.
const DARK_OS: SystemPreferences = { dark: true, moreContrast: false, reducedTransparency: false };

describe("systemNotes", () => {
  // Every combination of the two choices, on one OS.
  const table: [(typeof COLOR_SCHEMES)[number], (typeof CONTRAST_MODES)[number], string[]][] = [
    ["system", "system", ["Color scheme follows the system: Dark.", "Contrast mode follows the system: Glass."]],
    ["system", "default", ["Color scheme follows the system: Dark."]],
    ["system", "high-contrast", ["Color scheme follows the system: Dark."]],
    ["system", "glass", ["Color scheme follows the system: Dark."]],
    ["light", "system", ["Contrast mode follows the system: Glass."]],
    ["dark", "system", ["Contrast mode follows the system: Glass."]],
    ["light", "default", []],
    ["light", "high-contrast", []],
    ["light", "glass", []],
    ["dark", "default", []],
    ["dark", "high-contrast", []],
    ["dark", "glass", []],
  ];

  it.each(table)("color scheme %s, contrast mode %s", (scheme, contrast, expected) => {
    expect(systemNotes(scheme, contrast, resolveTheme(scheme, contrast, DARK_OS))).toEqual(expected);
  });

  it("the table holds every combination of the choices once", () => {
    const keys = table.map(([s, c]) => `${s}/${c}`);
    expect(new Set(keys).size).toBe(COLOR_SCHEMES.length * CONTRAST_MODES.length);
  });

  // What System resolved to, for every OS preference.
  it.each(ALL_SYSTEMS)("names what System resolved to on %o", (os) => {
    const scheme = `Color scheme follows the system: ${os.dark ? "Dark" : "Light"}.`;
    const contrast = `Contrast mode follows the system: ${os.moreContrast ? "High contrast" : os.reducedTransparency ? "Default" : "Glass"}.`;
    expect(systemNotes("system", "system", resolveTheme("system", "system", os))).toEqual([scheme, contrast]);
    expect(systemNotes("system", "high-contrast", resolveTheme("system", "high-contrast", os))).toEqual([scheme]);
    expect(systemNotes("dark", "system", resolveTheme("dark", "system", os))).toEqual([contrast]);
  });

  it("never describes an explicit choice as following the system", () => {
    for (const os of ALL_SYSTEMS) {
      for (const scheme of COLOR_SCHEMES) {
        for (const contrast of CONTRAST_MODES) {
          const notes = systemNotes(scheme, contrast, resolveTheme(scheme, contrast, os));
          expect(notes.some((n) => n.startsWith("Color scheme"))).toBe(scheme === "system");
          expect(notes.some((n) => n.startsWith("Contrast mode"))).toBe(contrast === "system");
        }
      }
    }
  });
});
