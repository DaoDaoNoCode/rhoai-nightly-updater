import type { ColorScheme, ContrastMode, EffectiveContrast, ResolvedTheme } from "./themeCore";

/** The choices as the appearance controls name them. */
export const SCHEME_LABELS: Record<ColorScheme, string> = { system: "System", light: "Light", dark: "Dark" };
export const CONTRAST_LABELS: Record<ContrastMode, string> = { system: "System", default: "Default", "high-contrast": "High contrast", glass: "Glass" };

/** What System resolved to, with the names of the explicit choices it matches. */
const RESOLVED_CONTRAST_LABELS: Record<EffectiveContrast, string> = { default: "Default", "high-contrast": "High contrast", glass: "Glass" };

/**
 * The lines under the appearance controls: one for each choice that
 * follows the operating system (is set to System), saying what it resolved
 * to. An explicit choice gets no line, so none when neither follows it.
 */
export function systemNotes(scheme: ColorScheme, contrast: ContrastMode, resolved: ResolvedTheme): string[] {
  const notes: string[] = [];
  if (scheme === "system") notes.push(`Color scheme follows the system: ${resolved.dark ? "Dark" : "Light"}.`);
  if (contrast === "system") notes.push(`Contrast mode follows the system: ${RESOLVED_CONTRAST_LABELS[resolved.contrast]}.`);
  return notes;
}
