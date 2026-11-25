import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { UserPermissions } from "../types";
import { getUserPermissions, isSessionExpired, onSessionExpired, toApiError, type ApiError } from "../services/api";
import { PERMISSION_RETRY_MS } from "../constants";
import { PollingAllowedContext, usePolling } from "../hooks/usePolling";
import { useLatestRequest } from "../hooks/useLatestRequest";

// ---------------------------------------------------------------------------
// Permissions and role
// ---------------------------------------------------------------------------

/**
 * checking: the first answer has not arrived. allowed / denied: the backend
 * answered. unknown: it could not answer (503 authorization_unavailable, a
 * network error, an expired session); mutations stay disabled, never "allowed".
 */
export type PermissionStatus = "checking" | "allowed" | "denied" | "unknown";

/**
 * Which view the app shows (docs/research/per-user-views.md): "admin" only
 * when the backend says so; "user" for every other answer, a missing role,
 * a refused or failed check included (fail closed); "checking" until the
 * first answer, when nothing role-specific shows.
 */
export type ViewRole = "admin" | "user" | "checking";

export interface PermissionsValue {
  status: PermissionStatus;
  canMutate: boolean;
  /** Why mutations are disabled for this user, or null when they are allowed. */
  reason: string | null;
  /** The backend's code for its reason (sign_in_again: the sign-in cannot carry a change), or null. */
  reasonCode: string | null;
  error: ApiError | null;
  retry: () => void;
}

export const NO_PERMISSION_REASON = "Read-only access: changing the cluster through this tool requires the cluster-admin role.";
export const PERMISSION_CHECKING_REASON = "Checking your permissions.";
export const PERMISSION_UNKNOWN_REASON = "Your permissions could not be checked, so changes are disabled. Retry from the banner at the top of the page.";
export const SESSION_EXPIRED_REASON = "Your session has expired. Sign in again to make changes.";

function permissionReason(status: PermissionStatus, sessionExpired: boolean, deniedReason: string | null = null): string | null {
  if (sessionExpired) return SESSION_EXPIRED_REASON;
  switch (status) {
    case "allowed": return null;
    case "denied": return deniedReason ?? NO_PERMISSION_REASON;
    case "checking": return PERMISSION_CHECKING_REASON;
    case "unknown": return PERMISSION_UNKNOWN_REASON;
  }
}

interface PermissionsContextValue {
  permissions: PermissionsValue;
  role: ViewRole;
  /** The signed-in user's name from the permission check, or null until it answered. */
  user: string | null;
  sessionExpired: boolean;
}

const PermissionsContext = createContext<PermissionsContextValue | null>(null);

function usePermissionsContext(): PermissionsContextValue {
  const value = useContext(PermissionsContext);
  if (!value) throw new Error("Permission hooks must be used inside PermissionsProvider");
  return value;
}

export function usePermissions(): PermissionsValue {
  return usePermissionsContext().permissions;
}

/** The view to show: admin, user, or checking (show nothing role-specific yet). */
export function useRole(): ViewRole {
  return usePermissionsContext().role;
}

/** The signed-in user's name, from the permission check. */
export function useSignedInUser(): string | null {
  return usePermissionsContext().user;
}

export function useSessionExpired(): boolean {
  return usePermissionsContext().sessionExpired;
}

interface PermissionsProviderProps {
  /** Injected for tests. */
  fetchPermissions?: (signal?: AbortSignal) => Promise<UserPermissions>;
}

/**
 * Who the user is and what they may do: the session, the permission check
 * (GET /api/user/permissions) and the role. Outermost of the app's state,
 * because the role decides which reads the other providers make (a user
 * never polls the cluster-admin's status).
 */
export const PermissionsProvider: React.FC<React.PropsWithChildren<PermissionsProviderProps>> = ({ children, fetchPermissions = getUserPermissions }) => {
  const fetcher = useRef(fetchPermissions);
  useEffect(() => {
    fetcher.current = fetchPermissions;
  });

  // --- Session ---------------------------------------------------------------
  const [sessionExpired, setSessionExpired] = useState(false);
  useEffect(() => onSessionExpired(() => setSessionExpired(true)), []);

  // --- Permissions -----------------------------------------------------------
  const [permStatus, setPermStatus] = useState<PermissionStatus>("checking");
  const [permError, setPermError] = useState<ApiError | null>(null);
  // The backend's reason when a cluster-admin's sign-in cannot carry a change.
  const [deniedReason, setDeniedReason] = useState<string | null>(null);
  const [deniedCode, setDeniedCode] = useState<string | null>(null);
  const [admin, setAdmin] = useState(false);
  const [user, setUser] = useState<string | null>(null);
  const permStatusRef = useRef(permStatus);
  permStatusRef.current = permStatus;

  const permRequest = useLatestRequest();
  const checkPermissions = useCallback(async () => {
    const ticket = permRequest.start();
    try {
      const p = await fetcher.current(ticket.signal);
      if (!ticket.isLatest()) return;
      setPermStatus(p.canMutate ? "allowed" : "denied");
      setDeniedReason(!p.canMutate && p.reason ? p.reason : null);
      setDeniedCode(!p.canMutate && p.reason ? p.reasonCode ?? null : null);
      setAdmin(p.role === "admin");
      setUser(typeof p.user === "string" && p.user ? p.user : null);
      setPermError(null);
    } catch (e) {
      if (!ticket.isLatest()) return;
      const err = toApiError(e, "Could not check your permissions");
      const expired = isSessionExpired(err);
      // A JSON 403 is an answer (read-only). An expired session (oauth-proxy's
      // HTML 403) and everything else, 503 authorization_unavailable
      // included, is "unknown": fail closed.
      setPermStatus(!expired && (err.status === 403 || err.errorCode === "forbidden") ? "denied" : "unknown");
      setDeniedReason(null);
      setDeniedCode(null);
      setAdmin(false);
      setPermError(err);
      if (expired) setSessionExpired(true);
    }
  }, [permRequest]);
  const permPoller = usePolling(checkPermissions, {
    // Retry soon while unanswered; otherwise re-check rarely (roles change).
    delay: () => (permStatusRef.current === "unknown" ? PERMISSION_RETRY_MS : 10 * 60_000),
    runImmediately: true,
    // Nothing answers until the user signs in again (a reload).
    enabled: !sessionExpired,
  });
  const retryPermissions = useCallback(() => {
    setPermStatus("checking");
    permPoller.reset({ runNow: true });
  }, [permPoller]);

  const role: ViewRole = permStatus === "checking" ? "checking" : admin ? "admin" : "user";

  const value = useMemo<PermissionsContextValue>(() => ({
    permissions: {
      status: permStatus,
      canMutate: permStatus === "allowed" && !sessionExpired,
      reason: permissionReason(permStatus, sessionExpired, deniedReason),
      reasonCode: permStatus === "denied" && !sessionExpired ? deniedCode : null,
      error: permError,
      retry: retryPermissions,
    },
    role,
    user,
    sessionExpired,
  }), [permStatus, sessionExpired, deniedReason, deniedCode, permError, retryPermissions, role, user]);

  return (
    <PermissionsContext.Provider value={value}>
      {/* An expired session stops every poll below. */}
      <PollingAllowedContext.Provider value={!sessionExpired}>{children}</PollingAllowedContext.Provider>
    </PermissionsContext.Provider>
  );
};
