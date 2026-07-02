/** Adaptive polling intervals while the cluster is reconciling an update (ms).
 *  Starts fast, then backs off: 5s for first 2 min, 10s for next 3 min, 15s after that. */
export const RECONCILE_POLL_FAST_MS = 5_000;
export const RECONCILE_POLL_MEDIUM_MS = 10_000;
export const RECONCILE_POLL_SLOW_MS = 15_000;

/** Thresholds for switching between adaptive polling tiers (ms of active polling time). */
export const RECONCILE_POLL_FAST_UNTIL_MS = 2 * 60_000;
export const RECONCILE_POLL_MEDIUM_UNTIL_MS = 5 * 60_000;

/** Legacy alias kept for backward compat (used in active-time delta guard). */
export const RECONCILE_POLL_MS = RECONCILE_POLL_SLOW_MS;

/** Maximum time to wait for reconciliation before giving up (ms). */
export const RECONCILE_TIMEOUT_MS = 10 * 60_000;

/** Background polling interval for general status freshness (ms). */
export const BACKGROUND_POLL_MS = 60_000;

/** Polling interval for the Components page (ms). */
export const COMPONENTS_POLL_MS = 30_000;

/** Navigation items for the sidebar. */
export const NAV_ITEMS = [
  { path: "/", label: "Dashboard" },
  { path: "/components", label: "Components" },
  { path: "/builds", label: "Build Explorer" },
  { path: "/dashboard-dev", label: "Dashboard Dev" },
  { path: "/diagnostics", label: "Diagnostics" },
] as const;
