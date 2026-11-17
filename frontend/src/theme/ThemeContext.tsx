import React, { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useState } from "react";
import {
  COLOR_SCHEME_KEY,
  CONTRAST_MODE_KEY,
  MEDIA_QUERIES,
  applyThemeClasses,
  readStoredTheme,
  resolveTheme,
  type ColorScheme,
  type ContrastMode,
  type ResolvedTheme,
  type SystemPreferences,
} from "./themeCore";

interface ThemeContextValue {
  colorScheme: ColorScheme;
  contrastMode: ContrastMode;
  /** What is shown now, after System is resolved against the OS. */
  resolved: ResolvedTheme;
  setColorScheme: (scheme: ColorScheme) => void;
  setContrastMode: (mode: ContrastMode) => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

function mediaQuery(query: string): MediaQueryList | null {
  try {
    return typeof window.matchMedia === "function" ? window.matchMedia(query) : null;
  } catch {
    return null;
  }
}

function systemPreferences(): SystemPreferences {
  return {
    dark: !!mediaQuery(MEDIA_QUERIES.dark)?.matches,
    moreContrast: !!mediaQuery(MEDIA_QUERIES.moreContrast)?.matches,
    reducedTransparency: !!mediaQuery(MEDIA_QUERIES.reducedTransparency)?.matches,
  };
}

function persist(key: string, value: string): void {
  try {
    storage()?.setItem(key, value);
  } catch {
    // Storage disabled: the choice lasts until the page is reloaded.
  }
}

/**
 * The color scheme and contrast mode, as stored, and the theme they resolve
 * to. It keeps the classes on <html> in step (public/theme-init.js set them
 * before the first paint) and follows OS changes live while a choice is
 * System.
 */
export const ThemeProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [initial] = useState(() => readStoredTheme(storage()));
  const [colorScheme, setScheme] = useState<ColorScheme>(initial.scheme);
  const [contrastMode, setContrast] = useState<ContrastMode>(initial.contrast);
  const [system, setSystem] = useState<SystemPreferences>(systemPreferences);

  useEffect(() => {
    const lists = Object.values(MEDIA_QUERIES).map(mediaQuery).filter((l): l is MediaQueryList => !!l);
    const onChange = () => setSystem(systemPreferences());
    lists.forEach((l) => l.addEventListener?.("change", onChange));
    return () => lists.forEach((l) => l.removeEventListener?.("change", onChange));
  }, []);

  const resolved = useMemo(() => resolveTheme(colorScheme, contrastMode, system), [colorScheme, contrastMode, system]);

  // A layout effect, not a passive one: the classes change the layout (the
  // glass theme sets PatternFly's page-chrome inset to 16 px instead of
  // 24 px, so everything in the masthead moves 8 px). React runs every
  // layout effect of a commit before any passive effect, so PatternFly's
  // Popper, which re-measures its trigger in a passive effect when the
  // popover's content changes (helpers/Popper/Popper.js), measures the new
  // layout. Applied in a passive effect, the classes changed after that
  // measurement, children's effects running before their parents'.
  useLayoutEffect(() => {
    applyThemeClasses(document.documentElement, resolved);
  }, [resolved]);

  const setColorScheme = useCallback((scheme: ColorScheme) => {
    setScheme(scheme);
    persist(COLOR_SCHEME_KEY, scheme);
  }, []);
  const setContrastMode = useCallback((mode: ContrastMode) => {
    setContrast(mode);
    persist(CONTRAST_MODE_KEY, mode);
  }, []);

  const value = useMemo(
    () => ({ colorScheme, contrastMode, resolved, setColorScheme, setContrastMode }),
    [colorScheme, contrastMode, resolved, setColorScheme, setContrastMode],
  );
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
};

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used inside ThemeProvider");
  return ctx;
}
