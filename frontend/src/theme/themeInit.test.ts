import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  COLOR_SCHEMES,
  COLOR_SCHEME_KEY,
  CONTRAST_MODES,
  CONTRAST_MODE_KEY,
  LEGACY_THEME_KEY,
  MEDIA_QUERIES,
  THEME_CLASSES,
  applyThemeClasses,
  readStoredTheme,
  resolveTheme,
  type SystemPreferences,
} from "./themeCore";
import { ALL_SYSTEMS } from "./themeTestUtils";

// public/theme-init.js applies the theme before the bundle loads. It repeats
// themeCore.ts in plain ES5 (the CSP forbids an inline script, and the file
// is not bundled), so this test runs it for every stored value and OS
// preference and compares the classes and storage with themeCore's.
const SOURCE = readFileSync(resolve(__dirname, "../../public/theme-init.js"), "utf8");

function stubSystem(system: SystemPreferences) {
  const answers: Record<string, boolean> = {
    [MEDIA_QUERIES.dark]: system.dark,
    [MEDIA_QUERIES.moreContrast]: system.moreContrast,
    [MEDIA_QUERIES.reducedTransparency]: system.reducedTransparency,
  };
  vi.stubGlobal("matchMedia", (q: string) => ({ matches: !!answers[q], media: q }));
}

function classes(root: Element): string {
  return Object.values(THEME_CLASSES).filter((c) => root.classList.contains(c)).sort().join(" ");
}

afterEach(() => {
  localStorage.clear();
  document.documentElement.className = "";
});

describe("public/theme-init.js matches themeCore", () => {
  const stored: (string | null)[] = [null, "junk"];
  for (const system of ALL_SYSTEMS) {
    for (const scheme of [...stored, ...COLOR_SCHEMES]) {
      for (const contrast of [...stored, ...CONTRAST_MODES]) {
        for (const legacy of [null, "dark", "light"]) {
          it(`scheme=${scheme} contrast=${contrast} pf-theme=${legacy} os=${JSON.stringify(system)}`, () => {
            const seed = () => {
              localStorage.clear();
              if (scheme !== null) localStorage.setItem(COLOR_SCHEME_KEY, scheme);
              if (contrast !== null) localStorage.setItem(CONTRAST_MODE_KEY, contrast);
              if (legacy !== null) localStorage.setItem(LEGACY_THEME_KEY, legacy);
            };
            stubSystem(system);

            seed();
            document.documentElement.className = "";
            new Function(SOURCE)();
            const fromScript = classes(document.documentElement);
            const storageAfterScript = JSON.stringify({ ...localStorage });

            seed();
            const expected = document.createElement("html");
            const choice = readStoredTheme(localStorage);
            applyThemeClasses(expected, resolveTheme(choice.scheme, choice.contrast, system));

            expect(fromScript).toBe(classes(expected));
            expect(storageAfterScript).toBe(JSON.stringify({ ...localStorage }));
          });
        }
      }
    }
  }

  it("still sets the felt theme when storage and matchMedia are unavailable", () => {
    vi.stubGlobal("matchMedia", undefined);
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
    new Function(SOURCE)();
    expect(classes(document.documentElement)).toBe("pf-v6-theme-felt pf-v6-theme-glass");
  });
});
