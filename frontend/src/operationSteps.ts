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

// Pipeline step definitions. They mirror the order and the step ids that
// pkg/cluster/operations.go emits (UpdateStreamWithOptions,
// ReinstallStreamWithOptions, RefreshOperatorStreamWithOptions), so the
// confirmation dialogs and the progress view describe what really runs.

const UPDATE_STEPS: PipelineStepDef[] = [
  { id: "validate_prerequisites", label: "Check prerequisites", description: "Pull secret, image mirror and OperatorGroup; verify the image in a temporary catalog; refuse an older build" },
  { id: "save_snapshot", label: "Save snapshot", description: "Record the current deployments to show what changed afterwards" },
  { id: "apply_catalog_source", label: "Replace the nightly catalog", description: "Remove the Subscription (the operator keeps running) and point the nightly CatalogSource at the new image" },
  { id: "wait_catalog_ready", label: "Wait for the catalog", description: "The catalog pod pulls the image and starts serving it" },
  { id: "detect_channel", label: "Detect the channel", description: "Pick the channel that carries this build" },
  { id: "delete_csv", label: "Remove the old operator version", description: "Delete the old CSV and InstallPlan so OLM installs the new build" },
  { id: "apply_subscription", label: "Create the Subscription", description: "Same settings as before (config, approval mode), new catalog and channel" },
  { id: "verify_installplan", label: "Wait for the operator install", description: "OLM installs the new CSV; this usually takes a few minutes (the tool waits up to 8)" },
];

const REINSTALL_COMMON_STEPS: PipelineStepDef[] = [
  { id: "save_snapshot", label: "Save snapshot", description: "Record the current deployments to show what changed afterwards" },
  { id: "delete_catalog_source", label: "Remove the nightly catalog", description: "Delete the nightly CatalogSource, if there is one" },
  { id: "delete_subscription", label: "Remove the Subscription", description: "Delete the operator Subscription" },
  { id: "delete_csv", label: "Remove the operator", description: "Delete the installed CSV; OLM removes the operator Deployment" },
  { id: "cleanup_webhooks", label: "Remove dead webhooks", description: "Remove webhook configurations whose service or operator no longer exists" },
  { id: "patch_crds", label: "Check CRD conversions", description: "Switch DSC/DSCI conversion to None only if no operator serves it any more" },
  { id: "wait_propagation", label: "Wait for cleanup", description: "Give the API server time to finish the deletions" },
];

const REINSTALL_STABLE_STEPS: PipelineStepDef[] = [
  { id: "validate_target", label: "Check the target", description: "Find the stable release in the cluster catalog and compare it with the installed version" },
  ...REINSTALL_COMMON_STEPS,
  { id: "create_subscription", label: "Create the Subscription", description: "Subscribe to the stable channel with the previous settings" },
  { id: "verify_installplan", label: "Wait for the operator install", description: "OLM installs the stable CSV; this usually takes a few minutes" },
];

const REINSTALL_NIGHTLY_STEPS: PipelineStepDef[] = [
  { id: "validate_target", label: "Check the target", description: "Verify the image in a temporary catalog and compare it with the installed version" },
  ...REINSTALL_COMMON_STEPS,
  { id: "create_catalog_source", label: "Create the nightly catalog", description: "Point the nightly CatalogSource at the selected image" },
  { id: "wait_catalog_ready", label: "Wait for the catalog", description: "The catalog pod pulls the image and starts serving it" },
  { id: "detect_channel", label: "Detect the channel", description: "Pick the channel that carries this build" },
  { id: "create_subscription", label: "Create the Subscription", description: "Subscribe to the nightly catalog with the previous settings" },
  { id: "verify_installplan", label: "Wait for the operator install", description: "OLM installs the new CSV; this usually takes a few minutes" },
];

const REFRESH_STEPS: PipelineStepDef[] = [
  { id: "verify_csv", label: "Check the operator", description: "Find the installed CSV" },
  { id: "save_snapshot", label: "Save snapshot", description: "Record the current deployments to show what changed afterwards" },
  { id: "get_subscription", label: "Read the Subscription", description: "Keep its catalog, channel, config and approval mode" },
  { id: "delete_subscription", label: "Remove the Subscription", description: "The operator keeps running meanwhile" },
  { id: "delete_csv", label: "Remove the operator", description: "Delete the installed CSV" },
  { id: "wait_cleanup", label: "Wait for cleanup", description: "Wait until the CSV is gone" },
  { id: "recreate_subscription", label: "Recreate the Subscription", description: "Same catalog, channel and settings as before" },
  { id: "verify_installplan", label: "Wait for the operator install", description: "OLM installs the same version again" },
];

/**
 * Shown after the regular steps when the backend undoes a failed attempt
 * (pkg/cluster/operator_recovery.go restoreStepName).
 */
export const RESTORE_STEP: PipelineStepDef = {
  id: "restore_previous_operator",
  label: "Restore the previous state",
  description: "Undo this attempt's changes after the failure",
};

/** Map operation type to its step definitions. */
export const STEP_SETS: Record<OperationKind, PipelineStepDef[]> = {
  update: UPDATE_STEPS,
  reinstall_stable: REINSTALL_STABLE_STEPS,
  reinstall_nightly: REINSTALL_NIGHTLY_STEPS,
  refresh: REFRESH_STEPS,
};

/** Map operation type to a human-readable pipeline title. */
export const PIPELINE_TITLES: Record<OperationKind, string> = {
  update: "Update",
  reinstall_stable: "Reinstall (stable)",
  reinstall_nightly: "Reinstall (nightly)",
  refresh: "Re-deploy operator",
};

/** Short operation names for status messages. */
export const OPERATION_NAMES: Record<OperationKind, string> = {
  update: "Update",
  reinstall_stable: "Reinstall (stable)",
  reinstall_nightly: "Reinstall (nightly)",
  refresh: "Operator re-deploy",
};

export function stepLabel(kind: OperationKind, stepId: string): string {
  if (stepId === RESTORE_STEP.id) return RESTORE_STEP.label;
  return STEP_SETS[kind].find((s) => s.id === stepId)?.label ?? stepId.replace(/_/g, " ");
}

/**
 * The streamed operation kind of a backend operation (GET /api/operation
 * `type`, pkg/api operationTypes), or null for operations without steps
 * (Dashboard Dev deploys, test resources, fixes...). A reinstall's target is
 * "stable" or "nightly <image>" / "custom <image>" (HandleReinstallStream).
 */
export function operationKindForServerType(type: string, target?: string): OperationKind | null {
  switch (type) {
    case "update":
      return "update";
    case "refresh":
      return "refresh";
    case "reinstall":
      return target?.startsWith("stable") ? "reinstall_stable" : "reinstall_nightly";
    default:
      return null;
  }
}
