import type { SystemPreferences } from "./themeCore";

/** Every combination of the three OS preferences, for the theme tests. */
export const ALL_SYSTEMS: SystemPreferences[] = [false, true].flatMap((dark) =>
  [false, true].flatMap((moreContrast) => [false, true].map((reducedTransparency) => ({ dark, moreContrast, reducedTransparency }))),
);
