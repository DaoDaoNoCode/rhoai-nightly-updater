/** Adaptive polling intervals while the cluster is reconciling an update (ms).
 *  Starts fast, then backs off: 5s for first 2 min, 10s for next 3 min, 15s after that. */
export const RECONCILE_POLL_FAST_MS = 5_000;
export const RECONCILE_POLL_MEDIUM_MS = 10_000;
export const RECONCILE_POLL_SLOW_MS = 15_000;

/** Thresholds for switching between adaptive polling tiers (ms of active polling time). */
export const RECONCILE_POLL_FAST_UNTIL_MS = 2 * 60_000;
export const RECONCILE_POLL_MEDIUM_UNTIL_MS = 5 * 60_000;

/** Maximum time to wait for reconciliation before giving up (ms). */
export const RECONCILE_TIMEOUT_MS = 10 * 60_000;

/** Background polling interval for general status freshness (ms). */
export const BACKGROUND_POLL_MS = 60_000;

/**
 * Longest time a progress stream is trusted (ms). The backend caps operations
 * at 15 minutes (withMutationAuth in pkg/api/handlers.go), so a stream still open after this
 * is abandoned and status polling takes over.
 */
export const STREAM_MAX_MS = 20 * 60_000;

/** Polling interval for the Components page (ms). */
export const COMPONENTS_POLL_MS = 30_000;

/** Test-resource status polling while something is starting: 5 s, doubling to 30 s. */
export const RESOURCE_POLL_BASE_MS = 5_000;
export const RESOURCE_POLL_MAX_MS = 30_000;
/** Stop polling a resource that is still not ready after this long (ms). */
export const RESOURCE_SETTLE_MAX_MS = 10 * 60_000;

/**
 * GET /api/operation polling. The endpoint makes no cluster call while an
 * operation runs and one ConfigMap read otherwise (pkg/api/operation.go), so
 * every tab can afford to watch for teammates' operations.
 */
export const OPERATION_POLL_BUSY_MS = 3_000;
export const OPERATION_POLL_IDLE_MS = 15_000;

/** Dashboard Dev session check for the global "dashboard-operator paused" banner (ms). */
export const DASHBOARD_OVERRIDE_POLL_MS = 120_000;
/** The backend caches the update check for 6 hours; a focused tab asks again after that. */
export const UPDATE_CHECK_REFRESH_MS = 6 * 60 * 60 * 1000;

/** Retry a permission check that could not be answered (503) after this long (ms). */
export const PERMISSION_RETRY_MS = 30_000;

/**
 * Navigation items for the sidebar. "Status" is this tool's home page;
 * "Dashboard Dev" deploys builds of the RHOAI (odh-dashboard) web UI.
 */
export const NAV_ITEMS = [
  { path: "/", label: "Status" },
  { path: "/components", label: "Components" },
  { path: "/builds", label: "Build Explorer" },
  { path: "/dashboard-dev", label: "Dashboard Dev" },
  { path: "/test-resources", label: "Test resources" },
  { path: "/diagnostics", label: "Diagnostics" },
] as const;
