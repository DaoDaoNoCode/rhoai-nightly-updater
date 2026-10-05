/** Operator operations that stream progress from the backend. */
export type OperationKind =
  | "update"
  | "reinstall_stable"
  | "reinstall_nightly"
  | "refresh";

/** Reconciliation tracking groups both reinstall kinds together. */
export type ReconcileKind = "update" | "refresh" | "reinstall";

export function reconcileKindFor(kind: OperationKind): ReconcileKind {
  return kind === "reinstall_stable" || kind === "reinstall_nightly" ? "reinstall" : kind;
}

export interface PipelineStepDef {
  id: string;
  label: string;
  description: string;
}

// Pipeline step definitions (mirror the backend execution order).

const UPDATE_STEPS: PipelineStepDef[] = [
  { id: "validate_prerequisites", label: "Validate Prerequisites", description: "Check pull secret, image mirror, and operator group" },
  { id: "save_snapshot", label: "Save Snapshot", description: "Record current deployment state for change detection" },
  { id: "apply_catalog_source", label: "Apply CatalogSource", description: "Create or update the nightly catalog with the FBC image" },
  { id: "wait_catalog_ready", label: "CatalogSource Loading", description: "Wait for the catalog pod to pull the image and serve content" },
  { id: "detect_channel", label: "Detect Channel", description: "Find the best channel from the catalog for this version" },
  { id: "apply_subscription", label: "Apply Subscription", description: "Point the operator subscription to the nightly catalog" },
  { id: "delete_csv", label: "Delete CSV", description: "Remove old operator to trigger fresh install" },
  { id: "verify_installplan", label: "Verify InstallPlan", description: "Confirm OLM created a new install plan" },
];

const REINSTALL_STABLE_STEPS: PipelineStepDef[] = [
  { id: "validate_target", label: "Validate Target", description: "Verify the target stable version is available" },
  { id: "save_snapshot", label: "Save Snapshot", description: "Record current deployment state before reinstall" },
  { id: "delete_catalog_source", label: "Delete CatalogSource", description: "Remove the nightly catalog source" },
  { id: "delete_subscription", label: "Delete Subscription", description: "Remove the current operator subscription" },
  { id: "delete_csv", label: "Delete CSV", description: "Remove the installed ClusterServiceVersion" },
  { id: "cleanup_webhooks", label: "Cleanup Webhooks", description: "Remove orphaned webhook configurations" },
  { id: "patch_crds", label: "Patch CRDs", description: "Remove owner references from CRDs to allow re-adoption" },
  { id: "wait_propagation", label: "Wait Propagation", description: "Wait for resource deletion to propagate across the cluster" },
  { id: "create_subscription", label: "Create Subscription", description: "Create a new subscription to the stable channel" },
  { id: "verify_installplan", label: "Verify InstallPlan", description: "Confirm OLM created a new install plan for the stable version" },
];

const REINSTALL_NIGHTLY_STEPS: PipelineStepDef[] = [
  { id: "validate_target", label: "Validate Target", description: "Verify the target nightly image is accessible" },
  { id: "save_snapshot", label: "Save Snapshot", description: "Record current deployment state before reinstall" },
  { id: "delete_catalog_source", label: "Delete CatalogSource", description: "Remove the existing catalog source" },
  { id: "delete_subscription", label: "Delete Subscription", description: "Remove the current operator subscription" },
  { id: "delete_csv", label: "Delete CSV", description: "Remove the installed ClusterServiceVersion" },
  { id: "cleanup_webhooks", label: "Cleanup Webhooks", description: "Remove orphaned webhook configurations" },
  { id: "patch_crds", label: "Patch CRDs", description: "Remove owner references from CRDs to allow re-adoption" },
  { id: "wait_propagation", label: "Wait Propagation", description: "Wait for resource deletion to propagate across the cluster" },
  { id: "create_catalog_source", label: "Create CatalogSource", description: "Create a new catalog source pointing to the nightly FBC image" },
  { id: "wait_catalog_ready", label: "CatalogSource Loading", description: "Wait for the catalog pod to pull the image and serve content" },
  { id: "detect_channel", label: "Detect Channel", description: "Find the best channel from the catalog for this version" },
  { id: "create_subscription", label: "Create Subscription", description: "Create a new subscription to the nightly catalog" },
  { id: "verify_installplan", label: "Verify InstallPlan", description: "Confirm OLM created a new install plan for the nightly version" },
];

const REFRESH_STEPS: PipelineStepDef[] = [
  { id: "verify_csv", label: "Verify CSV", description: "Check the current ClusterServiceVersion exists and is healthy" },
  { id: "save_snapshot", label: "Save Snapshot", description: "Record current deployment state before refresh" },
  { id: "get_subscription", label: "Get Subscription", description: "Retrieve current subscription details for recreation" },
  { id: "delete_csv", label: "Delete CSV", description: "Remove the installed ClusterServiceVersion" },
  { id: "delete_subscription", label: "Delete Subscription", description: "Remove the current operator subscription" },
  { id: "wait_cleanup", label: "Wait Cleanup", description: "Wait for OLM to finish cleaning up removed resources" },
  { id: "recreate_subscription", label: "Recreate Subscription", description: "Re-create the subscription to trigger a fresh install" },
  { id: "verify_installplan", label: "Verify InstallPlan", description: "Confirm OLM created a new install plan" },
];

/** Map operation type to its step definitions. */
export const STEP_SETS: Record<OperationKind, PipelineStepDef[]> = {
  update: UPDATE_STEPS,
  reinstall_stable: REINSTALL_STABLE_STEPS,
  reinstall_nightly: REINSTALL_NIGHTLY_STEPS,
  refresh: REFRESH_STEPS,
};

/** Map operation type to a human-readable pipeline title. */
export const PIPELINE_TITLES: Record<OperationKind, string> = {
  update: "Update Pipeline",
  reinstall_stable: "Reinstall Pipeline (Stable)",
  reinstall_nightly: "Reinstall Pipeline (Nightly)",
  refresh: "Refresh Pipeline",
};

/** Short operation names for status messages. */
export const OPERATION_NAMES: Record<OperationKind, string> = {
  update: "Update",
  reinstall_stable: "Reinstall (stable)",
  reinstall_nightly: "Reinstall (nightly)",
  refresh: "Operator refresh",
};

export function stepLabel(kind: OperationKind, stepId: string): string {
  return STEP_SETS[kind].find((s) => s.id === stepId)?.label ?? stepId.replace(/_/g, " ");
}
