import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ComponentInfo, ComponentsResponse, DSCCompatibility, DeploymentInfo } from "../types";
import { ApiError, getComponents, getComponentsWithLabels, toApiError } from "../services/api";
import { COMPONENTS_POLL_MS } from "../constants";
import { usePolling } from "./usePolling";

/** Git/build metadata that only `?labels=true` resolves (from image labels). */
export type DeploymentLabels = Pick<DeploymentInfo, "gitCommit" | "gitURL" | "commitDate" | "buildDate" | "version">;

/** Labels belong to an image, so the key includes it: a rollout to a new image invalidates them. */
export function labelKey(dep: Pick<DeploymentInfo, "namespace" | "name" | "image">): string {
  return `${dep.namespace}/${dep.name}@${dep.image}`;
}

export function extractLabels(resp: ComponentsResponse | null): Map<string, DeploymentLabels> {
  const map = new Map<string, DeploymentLabels>();
  for (const dep of resp?.deployments ?? []) {
    map.set(labelKey(dep), {
      gitCommit: dep.gitCommit,
      gitURL: dep.gitURL,
      commitDate: dep.commitDate,
      buildDate: dep.buildDate,
      version: dep.version,
    });
  }
  return map;
}

/** Overlay cached labels onto fresh deployment data; fresh fields always win. */
export function mergeDeploymentLabels(deployments: DeploymentInfo[], labels: Map<string, DeploymentLabels>): DeploymentInfo[] {
  return deployments.map((dep) => {
    const cached = labels.get(labelKey(dep));
    if (!cached) return dep;
    return {
      ...dep,
      gitCommit: dep.gitCommit || cached.gitCommit,
      gitURL: dep.gitURL || cached.gitURL,
      commitDate: dep.commitDate || cached.commitDate,
      buildDate: dep.buildDate || cached.buildDate,
      version: dep.version || cached.version,
    };
  });
}

/** Images in the data that have no labels yet (sorted, for a stable signature). */
function missingLabelKeys(resp: ComponentsResponse, labels: Map<string, DeploymentLabels>): string[] {
  return (resp.deployments ?? []).map(labelKey).filter((k) => !labels.has(k)).sort();
}

/** How long the first labels request may take before a fast plain request paints the page. */
export const LABELS_FIRST_PAINT_MS = 1_500;

/** ComponentsResponse with Go's null slices replaced by empty arrays. */
export type NormalizedDSCCompatibility = DSCCompatibility & { invalidFields: string[]; missingComponents: string[]; extraComponents: string[] };
export type NormalizedComponents = Omit<ComponentsResponse, "components" | "deployments" | "dscCompatibility"> & {
  components: ComponentInfo[];
  deployments: DeploymentInfo[];
  dscCompatibility?: NormalizedDSCCompatibility;
};

export interface ComponentsData {
  data: NormalizedComponents | null;
  loading: boolean;
  labelsLoading: boolean;
  error: ApiError | null;
  lastRefreshed: Date | null;
  /** Manual refresh: one labels request, which also refreshes the data. */
  refresh: () => void;
}

/**
 * Components page data. Polls `/api/components` every 30 s and overlays git
 * labels, keyed by deployment and image, from `?labels=true` responses, so
 * the Deployments table always shows the latest poll (A06-2, A03-5).
 *
 * On mount only the labels request is sent; it is a full, fresh response.
 * When the backend's label cache is cold it can take seconds, so a plain
 * request is started after LABELS_FIRST_PAINT_MS to paint the page. Labels
 * are fetched again only when a poll shows images without labels.
 */
export function useComponentsData(): ComponentsData {
  const [raw, setRaw] = useState<ComponentsResponse | null>(null);
  const [labels, setLabels] = useState<Map<string, DeploymentLabels>>(() => new Map());
  const [loading, setLoading] = useState(true);
  const [labelsLoading, setLabelsLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  const mounted = useRef(true);
  const seq = useRef(0); // request start order
  const appliedSeq = useRef(0); // newest response applied to `raw`
  const labelsInFlight = useRef(false);
  const attemptedLabelSets = useRef(new Set<string>());
  const labelsRef = useRef(labels);
  labelsRef.current = labels;

  const apply = useCallback((resp: ComponentsResponse, requestSeq: number) => {
    if (!mounted.current || requestSeq < appliedSeq.current) return false;
    appliedSeq.current = requestSeq;
    setRaw(resp);
    setError(null);
    setLastRefreshed(new Date());
    setLoading(false);
    return true;
  }, []);

  const fail = useCallback((e: unknown, requestSeq: number) => {
    if (!mounted.current || requestSeq < appliedSeq.current) return;
    setError(toApiError(e, "Failed to load components"));
    setLoading(false);
  }, []);

  const fetchPlain = useCallback(async () => {
    const requestSeq = ++seq.current;
    try {
      apply(await getComponents(), requestSeq);
    } catch (e) {
      fail(e, requestSeq);
    }
  }, [apply, fail]);

  // Returns true when the response was applied.
  const fetchLabels = useCallback(async (): Promise<boolean> => {
    if (labelsInFlight.current) return false;
    labelsInFlight.current = true;
    setLabelsLoading(true);
    const requestSeq = ++seq.current;
    try {
      const resp = await getComponentsWithLabels();
      if (!mounted.current) return false;
      // Labels are valid for their image whatever the response order.
      const fresh = extractLabels(resp);
      setLabels((prev) => new Map([...prev, ...fresh]));
      return apply(resp, requestSeq);
    } catch (e) {
      fail(e, requestSeq);
      return false;
    } finally {
      labelsInFlight.current = false;
      if (mounted.current) setLabelsLoading(false);
    }
  }, [apply, fail]);

  const poller = usePolling(fetchPlain, { delay: COMPONENTS_POLL_MS });

  useEffect(() => {
    mounted.current = true;
    let painted = false;
    const fallback = setTimeout(() => {
      if (!painted) void fetchPlain();
    }, LABELS_FIRST_PAINT_MS);
    void fetchLabels().then((applied) => {
      painted = true;
      clearTimeout(fallback);
      // A failed labels request still needs a first paint.
      if (!applied && appliedSeq.current === 0) void fetchPlain();
    });
    return () => {
      mounted.current = false;
      clearTimeout(fallback);
    };
  }, [fetchLabels, fetchPlain]);

  // A poll showed new images (for example after a rollout): resolve their labels once.
  useEffect(() => {
    if (!raw) return;
    const missing = missingLabelKeys(raw, labelsRef.current);
    if (missing.length === 0) return;
    const signature = missing.join("|");
    if (attemptedLabelSets.current.has(signature) || labelsInFlight.current) return;
    attemptedLabelSets.current.add(signature);
    void fetchLabels();
  }, [raw, fetchLabels]);

  const refresh = useCallback(() => {
    void fetchLabels().then(() => poller.reset());
  }, [fetchLabels, poller]);

  const data = useMemo<NormalizedComponents | null>(() => {
    if (!raw) return null;
    const compat = raw.dscCompatibility;
    return {
      ...raw,
      components: raw.components ?? [],
      deployments: mergeDeploymentLabels(raw.deployments ?? [], labels),
      dscCompatibility: compat ? {
        ...compat,
        invalidFields: compat.invalidFields ?? [],
        missingComponents: compat.missingComponents ?? [],
        extraComponents: compat.extraComponents ?? [],
      } : undefined,
    };
  }, [raw, labels]);

  return { data, loading, labelsLoading, error, lastRefreshed, refresh };
}
