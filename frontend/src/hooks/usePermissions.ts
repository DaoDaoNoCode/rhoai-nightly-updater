import { useEffect, useState } from "react";
import { getUserPermissions } from "../services/api";

export interface PermissionsState {
  canMutate: boolean;
  /** False until the backend answered (or failed). */
  loaded: boolean;
}

let cached: Promise<boolean> | null = null;

/** Forget the cached answer (tests, or after a sign-in change). */
export function resetPermissionsCache(): void {
  cached = null;
}

function loadCanMutate(): Promise<boolean> {
  if (!cached) {
    // Any failure, including 503 authorization_unavailable, means read-only:
    // the backend could not confirm the user may change the cluster.
    cached = getUserPermissions().then((p) => p.canMutate === true, () => {
      cached = null;
      return false;
    });
  }
  return cached;
}

/**
 * Whether the signed-in user may change the cluster, for pages that do not
 * receive `canMutate` from App. Read-only until the backend confirms.
 */
export function usePermissions(): PermissionsState {
  const [state, setState] = useState<PermissionsState>({ canMutate: false, loaded: false });
  useEffect(() => {
    let active = true;
    void loadCanMutate().then((canMutate) => {
      if (active) setState({ canMutate, loaded: true });
    });
    return () => { active = false; };
  }, []);
  return state;
}

export const CHECKING_PERMISSIONS_REASON = "Checking your permissions...";
