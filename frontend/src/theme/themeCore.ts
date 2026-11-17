/**
 * Theme resolution shared by the React ThemeProvider and public/theme-init.js
 * (the static script that applies the classes before the first paint; the
 * CSP allows no inline script). theme-init.js repeats this logic in plain
 * ES5, and themeInit.test.ts runs it against these functions for every
 * stored value and OS preference, so the two cannot drift.
 *
 * PatternFly 6.7 themes are classes on <html>: pf-v6-theme-felt (always on),
 * pf-v6-theme-dark, and either pf-v6-theme-glass or pf-v6-theme-high-contrast
 * (never both; high contrast wins).
 */

/** localStorage keys. Values are stored as plain strings. */
export const COLOR_SCHEME_KEY = "rhoai-updater.color-scheme";
export const CONTRAST_MODE_KEY = "rhoai-updater.contrast-mode";
/** The key of the light/dark toggle before 3.0.0 ("light" or "dark"). */
export const LEGACY_THEME_KEY = "pf-theme";

export const THEME_CLASSES = {
  felt: "pf-v6-theme-felt",
  dark: "pf-v6-theme-dark",
  glass: "pf-v6-theme-glass",
  highContrast: "pf-v6-theme-high-contrast",
} as const;

export const MEDIA_QUERIES = {
  dark: "(prefers-color-scheme: dark)",
  moreContrast: "(prefers-contrast: more)",
  reducedTransparency: "(prefers-reduced-transparency: reduce)",
} as const;

export type ColorScheme = "system" | "light" | "dark";
export type ContrastMode = "system" | "default" | "high-contrast" | "glass";
export type EffectiveContrast = "default" | "high-contrast" | "glass";

export const COLOR_SCHEMES: readonly ColorScheme[] = ["system", "light", "dark"];
export const CONTRAST_MODES: readonly ContrastMode[] = ["system", "default", "high-contrast", "glass"];

/** What the operating system asks for (CSS media features). */
export interface SystemPreferences {
  dark: boolean;
  moreContrast: boolean;
  reducedTransparency: boolean;
}

export interface ResolvedTheme {
  dark: boolean;
  contrast: EffectiveContrast;
}

type ThemeStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export function parseColorScheme(raw: string | null): ColorScheme {
  return raw === "light" || raw === "dark" ? raw : "system";
}

export function parseContrastMode(raw: string | null): ContrastMode {
  return raw === "default" || raw === "high-contrast" || raw === "glass" ? raw : "system";
}

/**
 * The theme to show. System follows the OS: dark with prefers-color-scheme:
 * dark; high contrast with prefers-contrast: more, else the solid default
 * with prefers-reduced-transparency: reduce, else glass. An explicit choice
 * wins over the OS.
 */
export function resolveTheme(scheme: ColorScheme, contrast: ContrastMode, system: SystemPreferences): ResolvedTheme {
  const dark = scheme === "dark" || (scheme === "system" && system.dark);
  let effective: EffectiveContrast;
  if (contrast === "system") {
    effective = system.moreContrast ? "high-contrast" : system.reducedTransparency ? "default" : "glass";
  } else {
    effective = contrast;
  }
  return { dark, contrast: effective };
}

/**
 * Moves the pre-3.0.0 light/dark choice to the color scheme key, once, and
 * removes the old key. A stored color scheme wins. No storage: nothing.
 */
export function migrateLegacyTheme(storage: ThemeStorage): void {
  try {
    const legacy = storage.getItem(LEGACY_THEME_KEY);
    if (legacy === null) return;
    if (storage.getItem(COLOR_SCHEME_KEY) === null && (legacy === "light" || legacy === "dark")) {
      storage.setItem(COLOR_SCHEME_KEY, legacy);
    }
    storage.removeItem(LEGACY_THEME_KEY);
  } catch {
    // Storage disabled: the theme follows the system.
  }
}

/** The stored choices (after migrating the legacy key); System when unset or unreadable. */
export function readStoredTheme(storage: ThemeStorage | null): { scheme: ColorScheme; contrast: ContrastMode } {
  if (!storage) return { scheme: "system", contrast: "system" };
  migrateLegacyTheme(storage);
  try {
    return { scheme: parseColorScheme(storage.getItem(COLOR_SCHEME_KEY)), contrast: parseContrastMode(storage.getItem(CONTRAST_MODE_KEY)) };
  } catch {
    return { scheme: "system", contrast: "system" };
  }
}

/** Sets the PatternFly theme classes on root (normally <html>). */
export function applyThemeClasses(root: Element, theme: ResolvedTheme): void {
  root.classList.add(THEME_CLASSES.felt);
  root.classList.toggle(THEME_CLASSES.dark, theme.dark);
  root.classList.toggle(THEME_CLASSES.glass, theme.contrast === "glass");
  root.classList.toggle(THEME_CLASSES.highContrast, theme.contrast === "high-contrast");
}
