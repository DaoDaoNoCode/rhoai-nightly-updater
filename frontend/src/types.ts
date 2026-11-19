// These types mirror the JSON of pkg/types/types.go (and pkg/cluster for
// diagnostics). A Go slice field without omitempty encodes nil as null, so
// such fields are typed `T[] | null` and read with `?? []`.

export interface ActivityEntry {
  timestamp: string;
  user: string;
  action: string;
  detail: string;
  success: boolean;
  /** Why a failed operation failed or was refused. */
  reason?: string;
  /** Read-time fields from the backend: a readable name for `action`. */
  label?: string;
  /** "operator" | "dashboard-dev" | "test-resources" | "setup" | "diagnostics" | "other". */
  category?: string;
  /** Operator actions only: "<tag> · <12-hex digest>". */
  build?: string;
}

export interface StatusResponse {
  cluster: ClusterInfo;
  subscription: SubscriptionInfo;
  csv: CSVInfo;
  catalogSource: CatalogSourceInfo;
  pullSecret: PullSecretInfo;
  imageMirror: ImageMirrorInfo;
  consoleURL?: string;
  /** The RHOAI dashboard's address (Dashboard CR status.url, else its Route); absent when unknown. */
  dashboardURL?: string;
  stableSource: string;
  stableChannel: string;
  stableVersion?: string;
  stableChannelPinned?: boolean;
  stableDiscoveryError?: string;
  activity?: ActivityEntry[];
  errors?: string[];
  dscExists: boolean;
  /** The Argo CD application that manages the DataScienceCluster (confirmed, or unconfirmed with a reason). */
  dscManagedBy?: string;
  /** What Argo CD said about the DataScienceCluster's tracking annotation. */
  dscArgoCD?: ArgoCDTracking;
  /** Installed vs latest nightly; only while the Subscription uses the nightly catalog. */
  nightly?: NightlyStatus;
  /**
   * What the nightly catalog serves on the Subscription's channel, compared
   * with the installed operator. Only while Argo CD manages the catalog or
   * the Subscription: Git then picks the catalog image, which can run ahead
   * of the installed operator (pkg/types CatalogServes).
   */
  catalogServes?: CatalogServes;
  installPlan?: InstallPlanInfo;
  /** The catalog source pod that serves the FBC index, when it exists (pkg/types CatalogPodInfo). */
  catalogPod?: CatalogPodInfo;
}

/** pkg/types CatalogPodInfo. */
export interface CatalogPodInfo {
  name: string;
  /** Pending, Running */
  phase: string;
  ready: boolean;
  restartCount: number;
}

export interface CatalogServes {
  channel: string;
  /** currentCSV of the channel. */
  csv: string;
  operatorImage?: string;
  /** createdAt of that build (RFC3339). */
  built?: string;
  /** Whether the installed operator is this build; absent when unknown. */
  installed?: boolean;
  /** OLM is replacing the catalog pod after an image change; `installed` is absent meanwhile. */
  updating?: boolean;
}

/** The latest OLM InstallPlan of the operator (pkg/types InstallPlanInfo). */
export interface InstallPlanInfo {
  name: string;
  /** Planning, Installing, Complete, Failed */
  phase: string;
  approved: boolean;
}

/** One nightly FBC catalog build (pkg/types NightlyBuild). */
export interface NightlyBuild {
  image: string;
  tag?: string;
  digest?: string;
  buildDate?: string;
  dashboardCommit?: string;
  dashboardGitURL?: string;
}

export interface NightlyStatus {
  installed?: NightlyBuild;
  latest?: NightlyBuild;
  /** Installed digest differs from the latest digest of the same tag; absent when unknown. */
  updateAvailable?: boolean;
  checkedAt?: string;
  /** Why `latest` is missing. */
  error?: string;
}

export interface ClusterInfo {
  server: string;
  version: string;
  user: string;
  /**
   * "hosted" (ROSA HCP, HyperShift), "managed" (on the cluster, run by Red
   * Hat: ROSA Classic, OpenShift Dedicated) or "self-managed"; absent when
   * it could not be read, and in the gitops profile.
   */
  controlPlane?: "hosted" | "managed" | "self-managed";
}

export interface SubscriptionInfo {
  name: string;
  /** Where the RHOAI operator's Subscription is (the operator runs there). */
  namespace?: string;
  source: string;
  channel: string;
  state: string;
  /** The Argo CD application that manages the Subscription (confirmed, or unconfirmed with a reason). */
  managedBy?: string;
  /** What Argo CD said about the Subscription's tracking annotation. */
  argoCD?: ArgoCDTracking;
}

/**
 * What the backend learnt from Argo CD about an object's
 * argocd.argoproj.io/tracking-id annotation (pkg/cluster/argocd.go):
 * - "managed": the Application lists the object among its resources;
 * - "stale": the annotation is left over (staleReason), the tool treats
 *   the object as not managed;
 * - "unconfirmed": Argo CD could not be asked or did not answer clearly
 *   (reason), the tool treats the object as managed.
 */
export interface ArgoCDTracking {
  /** As the annotation names it: "name", or "namespace_name" outside Argo CD's own namespace. */
  application: string;
  state: "managed" | "stale" | "unconfirmed";
  /** The namespace of the Application found. */
  namespace?: string;
  staleReason?: "argo-cd-not-installed" | "application-missing" | "not-in-resources";
  reason?: string;
  syncStatus?: string;
}

export interface CSVInfo {
  name: string;
  version: string;
  phase: string;
}

export interface CatalogSourceInfo {
  exists: boolean;
  name: string;
  image: string;
  state: string;
  /** The Argo CD application that manages the CatalogSource (confirmed, or unconfirmed with a reason). */
  managedBy?: string;
  /** No Argo CD application manages the catalog, and an ACM policy enforces its spec. */
  managedByACM?: boolean;
  /** What Argo CD said about the CatalogSource's tracking annotation. */
  argoCD?: ArgoCDTracking;
}

export interface PullSecretInfo {
  exists: boolean;
  valid: boolean;
  detail?: string;
  /**
   * Set when the nodes may not pull with additional-pull-secret: a
   * self-managed cluster, or one whose control plane could not be read.
   */
  nodes?: NodePullSecretInfo;
}

/** The nodes' side of the quay.io/rhoai credentials: openshift-config/pull-secret, which the updater cannot read. */
export interface NodePullSecretInfo {
  /** Adds the quay.io/rhoai entry to openshift-config/pull-secret (a cluster-admin runs it; it prompts for the token). */
  command: string;
  /** Removes whatever quay.io/rhoai entry is there (it asks first). */
  removeCommand: string;
  /** The nightly catalog is READY on its quay.io/rhoai image: the nodes have pulled from quay.io/rhoai. */
  pulled: boolean;
  /**
   * This tool's record that it added the quay.io/rhoai entry; absent when
   * it has none (an entry, if any, was set by someone else). The one-click
   * actions check the entry against it with the user's token.
   */
  addedByTool?: NodePullSecretRecord;
}

/** This tool's record of the quay.io/rhoai entry it added to openshift-config/pull-secret (never the token). */
export interface NodePullSecretRecord {
  at?: string;
  by?: string;
  /** The entry has the token saved under Pull secret now (false: Add updates it). */
  sameToken: boolean;
}

/** The one-click setup actions on a cluster whose control plane runs on the cluster. */
export type NodeSetupAction = "mirror-create" | "mirror-remove" | "pull-secret-add" | "pull-secret-remove";

/** What a one-click setup action would do (POST /api/setup/nodes/plan), worked out with the user's token and a dry run. */
export interface NodeSetupPlan {
  action: NodeSetupAction;
  controlPlane?: ClusterInfo["controlPlane"];
  ready: boolean;
  nothingToDo?: boolean;
  state: string;
  changes?: string[];
  nodeImpact?: string;
  undo?: string;
  /** Machine config pools that will not take the change yet (paused or degraded). */
  pools?: string[];
  refusal?: string;
  refusalCode?: "hosted" | "managed" | "unknown_topology" | "forbidden" | "foreign" | "foreign_entry" | "not_owned" | "unreadable" | "external_secrets" | "no_token" | "quay" | "conflict" | "failed";
  resourceVersion?: string;
  uid?: string;
  /** Pull secret actions: who set the quay.io/rhoai entry ("tool": this tool's record matches it). */
  entrySetBy?: "tool" | "other" | "none";
  /** The auths keys of the quay.io/rhoai entries there (registry names, never values). */
  existingKeys?: string[];
  /** Add was refused for an entry this tool did not add; a plan with replaceExisting replaces it. */
  replaceable?: boolean;
  /** The plan replaces an entry this tool did not add (the user acknowledged it). */
  replacing?: boolean;
}

export interface ImageMirrorInfo {
  exists: boolean;
  name: string;
  source: string;
}

export interface OperationResponse {
  success: boolean;
  message: string;
  /** null when the Go result left Logs unset. */
  logs: string[] | null;
  errorCode?: string;
}

export interface LatestNightlyResponse {
  tag: string;
  image: string;
  digest?: string;
  buildDate?: string;
  error?: string;
}

export interface NightlyTag {
  tag: string;
  image: string;
  buildDate?: string;
}

export interface NightlyTagsResponse {
  tags: NightlyTag[] | null;
}

export interface ComponentInfo {
  name: string;
  managementState: string;
  status: string;
  message?: string;
  /** Short classified cause of a not-ready component; Diagnostics has the fix. */
  cause?: string;
  fixAction?: string;
  fixTitle?: string;
  fixDescription?: string;
  fixConfirm?: string;
}

export interface DeploymentInfo {
  name: string;
  namespace: string;
  ready: number;
  desired: number;
  available: number;
  image: string;
  /** The digest-pinned reference the labels come from (a floating tag resolves through a running pod). */
  labelImage?: string;
  unavailableReplicas: number;
  updatedReplicas: number;
  rolloutStuck: boolean;
  rolloutMessage?: string;
  changeStatus?: "updated" | "new";
  pods?: PodInfo[];
  gitCommit?: string;
  gitURL?: string;
  commitDate?: string;
  buildDate?: string;
  version?: string;
}

export interface DSCCompatibility {
  operatorVersion?: string;
  branch?: string;
  sourceURL?: string;
  invalidFields: string[] | null;
  missingComponents: string[] | null;
  extraComponents?: string[] | null;
  validationError?: string;
  defaultsError?: string;
}

export interface ComponentsResponse {
  dscCompatibility?: DSCCompatibility;
  components: ComponentInfo[] | null;
  deployments: DeploymentInfo[] | null;
  dscName: string;
  dscPhase: string;
  dscReason?: string;
  snapshotTime?: string;
  changedCount: number;
  consoleURL?: string;
}

export interface RelatedImage {
  name: string;
  image: string;
  category?: string; // "core", "workbench", "pipeline", "training", "infra", "other"
  gitCommit?: string;
  gitURL?: string;
  commitDate?: string;
  buildDate?: string;
  version?: string;
}

export interface FBCContentResponse {
  tag: string;
  image: string;
  bundleName?: string;
  relatedImages: RelatedImage[] | null;
  categories?: Record<string, number>;
  error?: string;
}

export interface ContainerInfo {
  name: string;
  ready: boolean;
  restarts: number;
  state: string; // "running", "waiting", "terminated"
  reason?: string; // e.g. "CrashLoopBackOff"
}

export interface PodInfo {
  name: string;
  namespace: string;
  phase: string;
  node: string;
  ready: boolean;
  restarts: number;
  image: string;
  imageID: string;
  age: string;
  containers?: ContainerInfo[];
  podTemplateHash?: string;
  schedulingReason?: string;
  schedulingMessage?: string;
  gitCommit?: string;
  gitURL?: string;
  buildDate?: string;
  version?: string;
}

export interface UserPermissions {
  canMutate: boolean;
  user: string;
  /** Why a user who may change the cluster cannot now (their sign-in cannot carry a change: an old session, or one about to expire). */
  reason?: string;
  reasonCode?: string;
}

export interface DashboardState {
  operatorAvailable?: boolean;
  operatorPaused?: boolean;
  operatorError?: string;
  isDevMode?: boolean;
  devMode?: 'main' | 'pr';
  devImages?: DashboardDevImage[];
  devImagesMatchTarget?: boolean;
  allDevImagesReady?: boolean;
  defaultImagesRestored?: boolean;
  currentImage: string;
  deploymentMode?: string;
  isCustomPR: boolean;
  prNumber?: number;
  prContainers?: string[];
  managed: boolean;
  podStatus: string;
  podReady: boolean;
  containersReady: number;
  containersTotal: number;
  pods?: PodInfo[];
  rolloutPending: boolean;
  schedulingFailureReason?: string;
  canAssistRollout: boolean;
  dashboardURL?: string;
  /** Present whenever the dashboard module's operator exists; `active` while it is paused. */
  override?: DashboardOverride;
}

/** A paused dashboard module operator / Dashboard Dev session (pkg/types DashboardOverride). */
export interface DashboardOverride {
  active: boolean;
  operatorPaused: boolean;
  sessionRecorded: boolean;
  sessionError?: string;
  mode?: string;
  prNumber?: number;
  flavor?: string;
  startedAt?: string;
  startedBy?: string;
  updatedAt?: string;
  updatedBy?: string;
  releaseVersionAtStart?: string;
  releaseVersion?: string;
  /** RHOAI was updated while paused. */
  stale: boolean;
  staleReasons?: string[] | null;
  /** A Dashboard CR deletion is waiting for the paused operator. */
  dashboardDeleting: boolean;
  warnings?: string[] | null;
  lastAction?: { action: string; by: string; at: string; detail?: string };
}

export interface DashboardDevImage {
  deployment: string;
  container: string;
  envVar: string;
  repository: string;
  currentImage: string;
  defaultImage: string;
  ready: boolean;
  targetImage?: string;
  matchesTarget?: boolean;
}

export interface ResourcesStatus {
  /** The S3 storage (SeaweedFS in namespace seaweedfs); the key is kept from earlier versions. */
  minio: ResourceState;
  mlflow: ResourceState;
  pipelineServers: ResourceState[] | null;
  /** Storage an earlier version of this tool left in namespace minio, until it is migrated or torn down. */
  legacyStorage?: LegacyStorageState | null;
  /** Namespace minio exists but this tool did not make it: it is not treated as an earlier version's storage. */
  foreignLegacyNamespace?: boolean;
  /** Why Pipelines cannot be set up on this cluster (its DataScienceCluster component is off), or absent. */
  pipelinesBlocked?: ComponentBlock | null;
  /** Why MLflow cannot be set up on this cluster, or absent. */
  mlflowBlocked?: ComponentBlock | null;
  /** The RHOAI dashboard's address, when known. */
  dashboardUrl?: string;
  /** The inference gateway (absent from an older backend). */
  inferenceGateway?: InferenceGatewayState;
  /** What the Models feature needs, and its models (absent from an older backend). */
  models?: ModelsStatus;
}

// --- Samples (pkg/types SampleStatus, MLflowSamplesStatus, PipelineSampleSpec) ---

/** An MLflow sample's id (pkg/cluster mlflowSampleIDs, in their add order). */
export type MLflowSampleId = "genai-demo" | "training" | "mcp-server" | "skill";

/**
 * absent: not there; present: added; partial: part of it is missing
 * (missing names what); removed: deleted in MLflow, Add restores it;
 * foreign: an object of the sample's name exists that this tool did not
 * create (left alone); unsupported: this MLflow has no such API; unknown:
 * its state could not be read (message says why).
 */
export type SampleState = "absent" | "present" | "partial" | "removed" | "foreign" | "unsupported" | "unknown";

export type MLflowSampleAction = "add" | "remove" | "regenerate";

/** One MLflow sample in a project. */
export interface SampleStatus {
  id: MLflowSampleId;
  name: string;
  state: SampleState;
  message?: string;
  missing?: string[] | null;
  /** The sample in the MLflow UI, when there is one. */
  link?: string;
  /** What the state allows. */
  actions: MLflowSampleAction[];
  /** A cluster change Add and Regenerate make first instead of the sample (pkg/types SampleStatus.Repair). */
  repair?: string;
}

/** GET /api/resources/samples/mlflow?project=. */
export interface MLflowSamplesStatus {
  project: string;
  /** False while MLflow is not deployed, not ready or has no address; reason says which. */
  available: boolean;
  reason?: string;
  mlflowURL?: string;
  samples: SampleStatus[];
}

/** GET /api/resources/samples/pipeline/spec (pkg/types PipelineSampleSpec). */
export interface PipelineSampleSpec {
  name: string;
  /** The name to save the file as. */
  fileName: string;
  description: string;
  /** The image both steps run. */
  image: string;
  /** The compiled pipeline (KFP IR). */
  yaml: string;
}

/** A DataScienceCluster component a feature needs that is not turned on (pkg/types ComponentBlock). */
export interface ComponentBlock {
  /** Its key in spec.components ("aipipelines"). */
  component: string;
  /** The DataScienceCluster's name; absent without one. */
  dsc?: string;
  /** Its managementState as read ("Removed"). */
  state?: string;
  /** In plain words, with who can fix it. */
  reason: string;
}

/** The S3 storage an earlier version set up in namespace minio (pkg/types LegacyStorageState). */
export interface LegacyStorageState {
  namespace: string;
  /** "SeaweedFS" or "MinIO", or absent when neither server is left. */
  server?: string;
  ready: boolean;
  /** Its in-cluster S3 address, for example minio-service.minio.svc:9000. */
  endpoint: string;
  /** The objects this tool created there ("Deployment seaweedfs", ...). */
  objects: string[];
  dataPVCs?: KeptPVC[] | null;
  /** Data volumes no server serves, so their objects cannot be copied. */
  uncopiedPVCs?: string[] | null;
  /** Pipeline servers (namespace/name) that still point at it. */
  usedBy?: string[] | null;
  terminating?: boolean;
  /**
   * The next part of Migrate storage: "migrate" (set up the new storage,
   * copy the objects, move the pipeline servers) or "remove" (delete the
   * old storage). pkg/cluster legacyStorageState.
   */
  stage: "migrate" | "remove";
  /** Pipeline servers this tool created on the old storage: Migrate storage moves them. */
  repoint?: string[] | null;
  /** The other pipeline servers on the old storage, each with why the tool leaves it. */
  notMoved?: string[] | null;
  /**
   * Those of repoint that carry this tool's label but whose UID this tool has no record of (set up before 3.0.1, or labelled by a
   * project's editor): Migrate storage moves one only when the user confirms its project (confirmProjects).
   */
  unrecorded?: string[] | null;
  /** Why the old storage cannot be removed yet. */
  removeBlockedReason?: string;
  /** True when namespace minio carries this tool's label and the tool wrote it: removing the old storage offers to delete it. */
  namespaceDeletable: boolean;
  /** Deployments and PVCs in the namespace this tool did not create (it cannot list every kind). */
  otherObjects?: string[] | null;
  /** Why the namespace is not offered for deletion. */
  namespaceKeptReason?: string;
}

export interface ResourceState {
  deployed: boolean;
  ready: boolean;
  message?: string;
  namespace?: string;
  apiRoute?: string;
  uiRoute?: string;
  currentImage?: string;
  /** Planned backend field: a container waiting reason such as ImagePullBackOff. */
  waitingReason?: string;
  /** A notice that needs action but does not stop the resource (for example another S3 storage image). */
  warning?: string;
}

export interface UpdateStep {
  step: string;
  status: 'running' | 'success' | 'failed' | 'skipped';
  message: string;
  detail?: string;
  elapsedMs: number;
  errorCode?: string;
}

// --- Diagnostics types (match backend cluster.DiagnosticResult JSON) ---

export interface DiagnosticResult {
  problems: Problem[] | null;
  checks: CheckResult[] | null;
}

export interface Problem {
  id: string;
  severity: "critical" | "warning" | "info";
  title: string;
  description: string;
  evidence?: string[];
  fix?: string;
  autoFixable?: boolean;
  autoFixAction?: string;
  confirmMessage?: string;
  learnMore?: string;
  technicalCmd?: string;
  /** "<Kind> <ns>/<name>" or "<Kind> <name>"; for auto-fixes, exactly what the fix may change. */
  affectedObjects?: string[] | null;
  /** IDs of other problems in the same report that fix this one's cause. */
  relatedProblems?: string[] | null;
  /** Per-field plan of a fix that changes fields of an object. */
  fieldChanges?: FieldChange[] | null;
  /** A statement the user must confirm before the fix runs (sent to /api/diagnostics/fix/acknowledged). */
  acknowledge?: string;
}

/** One field of a fix's plan (backend cluster.FieldChange). */
export interface FieldChange {
  object: string;
  field: string;
  /** The stored value as JSON; "" when the object no longer has the field. */
  now: string;
  /** The value after the fix as JSON; "" when removed or not changed. */
  after: string;
  change: "set-default" | "remove" | "none" | "valid";
  /** What the installed CRD says about the field. */
  schema: string;
  note?: string;
  /** The stored value may not mean the same as the new one. */
  review?: boolean;
}

export interface CheckResult {
  name: string;
  status: "pass" | "fail" | "warn" | "info";
  detail: string;
}

// ---------------------------------------------------------------------------
// Secondary pages (Components, Build Explorer, Dashboard Dev, test resources,
// Diagnostics): fields added by the backend fixes (B2-B5). Additive only:
// these declarations merge with the interfaces above (TypeScript interface
// merging), so the shared definitions stay untouched.
// ---------------------------------------------------------------------------

/** One FBC build as reported by `status.nightly` (pkg/types NightlyBuild). */
export interface NightlyBuild {
  image: string;
  tag?: string;
  digest?: string;
  buildDate?: string;
  dashboardCommit?: string;
  dashboardGitURL?: string;
}

/** Installed vs latest nightly (pkg/types NightlyStatus). */
export interface NightlyStatus {
  installed?: NightlyBuild;
  latest?: NightlyBuild;
  /** Absent when either digest is unknown. */
  updateAvailable?: boolean;
  checkedAt?: string;
  error?: string;
}

export interface StatusResponse {
  /** Omitted unless the Subscription uses the nightly catalog. */
  nightly?: NightlyStatus;
}

export interface DSCCompatibility {
  defaultsSource?: string;
  /** apiVersion of the defaults: always the one the DSC was read at (pkg/types DSCCompatibility). */
  defaultsAPIVersion?: string;
  /** Enabled components "reset-defaults" would set to Removed or drop (FXA). */
  resetRemovals?: string[] | null;
  /** Components a repair would remove that must not be removed now, with why (FXA). */
  removalBlocks?: DSCRemovalBlock[] | null;
}

export interface DSCRemovalBlock {
  component: string;
  reasons: string[];
}

/** A resource that stops working while a hard reset has the components it needs down (pkg/types DSCStopsWorking). */
export interface DSCStopsWorking {
  /** e.g. "pipeline server team/dspa". */
  resource: string;
  components: string[];
}

/** A CRD whose conversion webhook a component serves (pkg/types DSCConversionCRD). */
export interface DSCConversionCRD {
  crd: string;
  component: string;
}

/** One DSC status condition (pkg/types DSCConditionView). */
export interface DSCConditionView {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

/** A component or sub-component of spec.components as the installed CRD defines it (pkg/types DSCNode). */
export interface DSCNode {
  path: string;
  name: string;
  parent: string;
  depth: number;
  /** Has its own managementState; a group (dashboard, data) only holds sub-components. */
  toggle: boolean;
  allowed?: string[];
  schemaDefault?: string;
  spec?: string;
  specSet: boolean;
  default?: string;
  defaultSet: boolean;
  actual?: string;
  condition?: DSCConditionView;
  changed?: boolean;
  /** A field in the DSC the installed CRD does not define (an "unknown-field" mismatch); name is its path below parent. */
  unknown?: boolean;
}

/** What the comparison with the release default covered (pkg/types DSCComparison). */
export interface DSCComparison {
  /** The release default was read: missing values and changes from it were looked for. */
  defaultsRead: boolean;
  /** The CRD schema was read: fields it does not define were looked for. */
  schemaRead: boolean;
  /** The installed CSV whose alm-examples hold the release default. */
  defaultsCSV?: string;
  /** Keys of spec.components in the CRD, the release default and the DSC, sorted. */
  crdComponents: string[] | null;
  defaultComponents: string[] | null;
  dscComponents: string[] | null;
}

/** A difference from the release default that is a real problem (pkg/types DSCMismatch). */
export interface DSCMismatch {
  kind: "unset" | "parent-removed" | "unknown-field";
  path: string;
  default?: string;
  parent?: string;
  message: string;
  fixable: boolean;
}

/** GET /api/components/dsc (pkg/types DSCManager). */
export interface DSCManager {
  exists: boolean;
  state: string;
  name?: string;
  uid?: string;
  apiVersion?: string;
  storageVersion?: string;
  dscCount: number;
  phase?: string;
  releaseName?: string;
  releaseVersion?: string;
  operatorVersion?: string;
  defaultsSource?: string;
  defaultsError?: string;
  schemaError?: string;
  /** Why this page changes nothing in the DSC now; empty when it may. */
  readOnlyReason?: string;
  managedBy?: string;
  acmEnforced?: boolean;
  deleting?: boolean;
  finalizers?: string[];
  conditions: DSCConditionView[];
  upgradeGates?: string[];
  nodes: DSCNode[];
  mismatches: DSCMismatch[];
  /** What the comparison covered; absent when there is no DSC. */
  comparison?: DSCComparison;
  /** A hard reset that deleted the DSC and did not create its replacement; only while no DSC exists. */
  pendingHardReset?: DSCPendingHardReset;
  pendingHardResetError?: string;
}

/** The DataScienceCluster a stopped hard reset recorded; "Finish the hard reset" creates exactly it. */
export interface DSCPendingHardReset {
  name: string;
  apiVersion: string;
  newObject: string;
  operatorVersion?: string;
  user?: string;
  startedAt: string;
  token: string;
}

/** One change of a plan (pkg/types DSCChange). */
export interface DSCChange {
  path: string;
  from?: string;
  fromSet: boolean;
  to?: string;
  toSet: boolean;
  toggle: boolean;
  /** "deploys" or "removes" when what the operator deploys changes. */
  effect?: "deploys" | "removes" | "";
}

export type DSCPlanAction = "toggle" | "fix" | "soft-reset" | "hard-reset";

/** GET /api/components/dsc/plan (pkg/types DSCPlan). */
export interface DSCPlan {
  action: DSCPlanAction;
  name: string;
  uid?: string;
  apiVersion?: string;
  refusal?: string;
  code?: string;
  parentPath?: string;
  changes: DSCChange[];
  blocked?: DSCRemovalBlock[] | null;
  warnings?: string[] | null;
  /** Hard reset: what stops working while the components are down, each resource once. */
  stopsWorking?: DSCStopsWorking[] | null;
  /** Hard reset: the CRDs that cannot be read at another version while the component serving their conversion webhook is down. */
  conversionCRDs?: DSCConversionCRD[] | null;
  tornDown?: string[] | null;
  stayRemoved?: string[] | null;
  /** Hard reset: the object it creates, with the values the API server fills in from the CRD's defaults. */
  newObject?: string;
  /** Hard reset: how long it waits for the old DataScienceCluster to be gone before it stops without creating anything. */
  deleteTimeoutSeconds?: number;
  /** Resets: the fields whose value the reset keeps (the CRD does not let them change), where the DataScienceCluster sets them. */
  protectedFields?: string[] | null;
  /** Resets: the option the plan was made with (part of the token). */
  keepEnabled?: boolean;
  /** Resets: the toggles the user turned on (enabled now, off in the release default), with the parents they need. */
  turnedOn?: string[] | null;
  /** Resets: those the reset keeps at their live value (turnedOn with keepEnabled). */
  kept?: string[] | null;
  token?: string;
}

export interface ComponentsResponse {
  dscExists?: boolean;
  /** "present", "no-dsc" (CRD installed, no DSC) or "no-crd" (no DSC API). */
  dscState?: string;
  operatorVersion?: string;
  operatorPhase?: string;
  /** The installed CSV's containerImage and createdAt annotations: the operator build, from the cluster. */
  operatorImage?: string;
  operatorBuilt?: string;
  /** The gitops profile: image labels (commit, build date, version) are not read, since it contacts no registry. */
  imageLabelsOff?: boolean;
  /** The apiVersion the DSC was read at, e.g. "datasciencecluster.opendatahub.io/v3". */
  dscAPIVersion?: string;
  /** Set when the DSC could only be read at an older version than the preferred one. */
  dscVersionFallback?: DSCVersionFallback;
  /** RHOAI's applications namespace (the DSCInitialization's) and the operator's. */
  applicationsNamespace?: string;
  operatorNamespace?: string;
  /** Why the applications namespace could not be read; its Deployments are missing then. */
  applicationsNamespaceError?: string;
}

/** The DSC was read at an older API version because conversion to a newer one failed (pkg/types DSCVersionFallback). */
export interface DSCVersionFallback {
  /** The version that failed, e.g. "v3". */
  version: string;
  /** The version it was read at, e.g. "v2". */
  used: string;
  /** The API server's error message. */
  message: string;
}

/** Who started a Dashboard Dev action and when (pkg/types DashboardDevAction). */
export interface DashboardDevAction {
  /** "deploy-pr", "deploy-main" or "revert". */
  action: string;
  by: string;
  at: string;
  detail?: string;
}

/** The image the tool set on one dashboard container (pkg/types DashboardOverrideComponent). */
export interface DashboardOverrideComponent {
  deployment: string;
  container: string;
  image?: string;
  /** "pr", "main" or "baseline" (the release image). */
  source?: string;
  tag?: string;
}

/** The Dashboard Dev session recorded on the dashboard module's operator (pkg/types DashboardOverride). */
export interface DashboardOverride {
  active: boolean;
  operatorPaused: boolean;
  /** The paused Deployment, discovered at run time (e.g. dashboard-operator on RHOAI 3.6). */
  operatorName?: string;
  operatorNamespace?: string;
  sessionRecorded: boolean;
  sessionError?: string;
  /** "pr" or "main". */
  mode?: string;
  prNumber?: number;
  /** "rhoai" (Konflux, odh-pr-N) or "odh" (OpenShift CI, pr-N). */
  flavor?: string;
  startedAt?: string;
  startedBy?: string;
  updatedAt?: string;
  updatedBy?: string;
  releaseVersionAtStart?: string;
  releaseVersion?: string;
  /** RHOAI changed while the operator was paused. */
  stale: boolean;
  staleReasons?: string[] | null;
  /** The Dashboard CR is being deleted and waits for the paused operator. */
  dashboardDeleting: boolean;
  warnings?: string[] | null;
  components?: DashboardOverrideComponent[] | null;
  lastAction?: DashboardDevAction;
}

export interface DashboardState {
  override?: DashboardOverride;
  /** The dashboard module's operator Deployment, discovered at run time. */
  operatorName?: string;
  operatorNamespace?: string;
  /** Why the cluster has no dashboard module (the legacy flow). */
  noOperatorReason?: string;
  /** The applications namespace (the DSCInitialization's). */
  namespace?: string;
  flavor?: string;
  defaultFlavor?: string;
  availableFlavors?: string[] | null;
  /** A workload hit ProgressDeadlineExceeded (server-side condition). */
  rolloutStuck?: boolean;
  stuckReason?: string;
}

export interface DashboardDevImage {
  /** "pr", "main" or "baseline". */
  targetSource?: string;
  targetTag?: string;
  /** "release", "pr", "main" or "other". */
  running?: string;
  runningPR?: number;
  flavor?: string;
  baselineImage?: string;
  rolloutStuck?: boolean;
  rolloutMessage?: string;
  waitingReason?: string;
  waitingMessage?: string;
  restarts?: number;
  podName?: string;
}

/** GET /api/dashboard/builds (pkg/types DashboardBuildsResponse): the commit of every PR or main build the dashboard runs. */
export interface DashboardBuildsResponse {
  builds: DashboardContainerBuild[];
  /** GitHub is rate limited: divergences may be unknown until retryAfterSeconds. */
  rateLimited?: boolean;
  retryAfterSeconds?: number;
}

/** The PR or main build one dashboard container runs (pkg/types DashboardContainerBuild). */
export interface DashboardContainerBuild {
  deployment: string;
  container: string;
  /** The running image, as in DashboardDevImage.currentImage. */
  image: string;
  /** "pr" or "main". */
  running: string;
  pr?: number;
  /** "rhoai" or "odh". */
  flavor?: string;
  /** 40 hex, from the image labels. */
  commit?: string;
  /** "opendatahub-io/odh-dashboard". */
  repo?: string;
  commitURL?: string;
  buildDate?: string;
  commitDate?: string;
  divergence?: DashboardBuildDivergence;
  /** The floating tag this build came from now points to another build. */
  newer?: DashboardNewerBuild;
  /** Why fields are missing: "no_commit_label", "registry_error", "unknown_repo". */
  reason?: string;
  message?: string;
}

/** How a build's commit relates to the branch it tracks (pkg/types DashboardBuildDivergence). */
export interface DashboardBuildDivergence {
  /** "main" or "pr-<N>". */
  base: string;
  baseHead?: string;
  /** The build against the base: "identical", "behind", "ahead", "diverged" or "unknown". */
  status: string;
  /** Commits on the base the build lacks. */
  behind: number;
  /** Commits in the build the base lacks. */
  ahead: number;
  compareURL?: string;
  /** PR builds: "open", "merged" or "closed". */
  prState?: string;
  prTitle?: string;
  checkedAt?: string;
  /** Why "unknown": "rate_limited", "commit_not_found", "github_error", "pr_not_found". */
  reason?: string;
  message?: string;
}

/** The build a floating tag points to now (pkg/types DashboardNewerBuild). */
export interface DashboardNewerBuild {
  /** Digest-pinned, as Deploy would write it. */
  image: string;
  /** The floating tag checked: odh-stable, main, odh-pr-N or pr-N. */
  tag: string;
  commit?: string;
  commitURL?: string;
  buildDate?: string;
}

export interface ResourcesStatus {
  /** Projects that already have a pipeline server the tool did not create. */
  unmanagedPipelineProjects?: string[] | null;
  /** The health of those pipeline servers (managedByTool false); the tool never changes them. */
  unmanagedPipelineServers?: ResourceState[] | null;
}

export interface ResourceState {
  /** DSPA name for pipeline servers: "dspa" (the dashboard's name, which workbench pipelines need), or "nightly-dspa" (set up by an earlier version). */
  name?: string;
  /** False: the tool did not create it and will not tear it down. */
  managedByTool?: boolean;
  /** A pipeline server with this tool's label but no record of its UID (set up before 3.0.1, or labelled by a project's editor): Set up again adopts it. */
  unrecorded?: boolean;
  terminating?: boolean;
  setupBlockedReason?: string;
  teardownBlockedReason?: string;
  /** The S3 storage: the pipeline servers ("namespace/name") that store artifacts in it, whoever created them. */
  usedBy?: string[] | null;
  /** The resource can't become ready without a change (for example ImagePullBackOff). */
  terminalError?: boolean;
  /** PVCs deleted with the resource. */
  dataPVCs?: string[] | null;
  /** MLflow runs a PR image (independent of readiness). */
  prOverride?: boolean;
  prNumber?: number;
  /** Image restored by Revert; "" means the operator default. */
  revertImage?: string;
  /** Login name of the resource's web UI (the SeaweedFS admin UI: "admin"). */
  uiUser?: string;
  /** Tear down may also delete the resource's namespace, which this tool created (the S3 storage's seaweedfs). */
  namespaceDeletable?: boolean;
  /** Why that namespace is not offered for deletion. */
  namespaceKeptReason?: string;
  /** Deployments and PVCs in that namespace this tool did not create (it cannot list every kind). */
  otherObjects?: string[] | null;
  /** Storage from an earlier version is left in namespace minio (ResourcesStatus.legacyStorage); Migrate storage moves it. */
  migrationPending?: boolean;
  /**
   * What re-running setup (Repair) would fix: the server does not serve
   * through its Service or Route, is scaled to zero, or a migration left
   * cleanup undone. Not ready: incomplete; ready: degraded.
   */
  repairNeeded?: string;
  /** It exists but reports no readiness this tool can read: its health is unknown, not failed. */
  healthUnknown?: boolean;
  /** A pipeline server set up under the earlier name nightly-dspa: workbench pipelines need "dspa" (pkg/cluster dspaName), so Recreate is offered. */
  nonStandardName?: boolean;
  /** The dashboard S3 Connection (a Secret in the project) set up with this pipeline server (pkg/cluster connectionSecretName), when there is one. */
  connection?: string;
  /** What the Connection Secret itself says, read only for the selected project (GET /api/resources/status?project=): present, missing, foreign or unreadable; absent when not read. */
  connectionState?: "present" | "missing" | "foreign" | "unreadable";
  connectionMessage?: string;
}

// --- Models (pkg/types InferenceGatewayState, ModelsStatus, ModelState) ---

/** The cluster's inference gateway (Gateway openshift-ingress/openshift-ai-inference). usedBy: the models ("namespace/name") that route through it. */
export interface InferenceGatewayState extends ResourceState {
  /** https://<hostname> of the listener for callers outside the cluster. */
  externalUrl?: string;
  /** While not set up: the address set up would give it. */
  plannedUrl?: string;
  /** The load balancer address the gateway reports. */
  address?: string;
  /** "published" once the external hostname is in DNS, "pending" before; absent: unknown. */
  dns?: "published" | "pending";
  /** The gateway's authentication (odh-model-controller adds it once a model uses the gateway); absent: unknown. */
  authPolicy?: "enforced" | "not-enforced" | "missing";
  /**
   * The projects the gateway's listeners list by name (RHOAI's documented
   * allowedRoutes values array). Absent: they choose otherwise (every
   * project, or labels).
   */
  allowedProjects?: string[];
  /** The listeners accept routes from every project. */
  allProjectsAllowed?: boolean;
  /** Projects whose models (anyone's, such as the dashboard's) route through the gateway but are not allowed: they do not attach. */
  notAllowedModelProjects?: string[];
  /** Those models, "namespace/name": their routes are rejected, so they never become Ready. */
  notAllowedModels?: string[];
  // --- Models routing ---
  /** Projects on this tool's gateway's list that no model routes through from (their editors can still attach routes). */
  unusedProjects?: string[];
  /** Of those, the ones that no longer exist: a new project of that name would be admitted at once. */
  goneProjects?: string[];
  /** The projects on the list this tool added: deleting their last model on the gateway takes them off. */
  addedProjects?: string[];
}

/** A model the Models feature sets up (pkg/cluster modelCatalog). */
export interface CatalogModel {
  /** The LLMInferenceService's name, and the model name callers send. */
  name: string;
  displayName: string;
  /** The Hugging Face repository, pinned to a commit (hf://<org>/<repo>:<commit>). */
  uri: string;
  cpu: string;
  /** What the scheduler reserves; memory is the limit. */
  memoryRequest: string;
  memory: string;
  type: "chat" | "embedding";
  /** A chat model served with tool calling turned on. */
  toolCalling?: boolean;
  /** An embedding model's vector size. */
  embeddingDimension?: number;
  /** What it is for ("Chat, Tool calling, Reasoning"). */
  useCase: string;
  description: string;
  /** The vLLM arguments it is served with. */
  args: string;
  /** Roughly what is downloaded ("270 MB"). */
  downloadSize: string;
  /** The model a request that names none sets up. */
  default?: boolean;
  /** No worker seems to have room for it now. */
  capacityWarning?: string;
}

/** One model of the catalog in a project (namespace, name). */
export interface ModelState extends ResourceState {
  displayName?: string;
  /** The model's address: the base of its OpenAI API. */
  url?: string;
  /** Which image it runs: the preset's, the newest released one, or VLLM_CPU_IMAGE's. */
  imageKind?: "preset" | "released" | "override";
  imageVersion?: string;
  /** The dashboard hardware profile it is recorded under ("<namespace>/<name>"), when one fits. */
  hardwareProfile?: string;
  /** The preset's image is not in the registry (a nightly build's): setting it up with the newest released image fixes it. */
  presetImageMissing?: boolean;
  /** With presetImageMissing: a registry answered "manifest unknown" while another refused the node's credentials, so the image may be missing or the node may lack the pull secret. */
  presetImageUncertain?: boolean;
  /** Routes through the MaaS gateway (published on the MaaS tab): used through MaaS, and its direct address answers 404. */
  maasPublished?: boolean;
  /** Set up on the Models tab "Through MaaS only", published as part of that setup: Delete unpublishes it first, and Unpublish leaves it with no route (only inside the cluster). */
  maasOnlySetup?: boolean;
  /** The model's token Secret and ServiceAccount, made as the dashboard's "Require token authentication" does; absent for a model set up before (set it up again). */
  tokenSecret?: string;
  tokenServiceAccount?: string;
  /** security.opendatahub.io/enable-auth is "false" on the model (turned off in the dashboard). */
  tokenAuthOff?: boolean;
  // --- Models routing ---
  /** How the model is reached, from its spec.router. */
  exposure?: ModelExposureKind;
  /** The gateway it routes through ("namespace/name"). */
  gateway?: string;
  /** Authentication on that gateway: "maas" means MaaS does it. */
  gatewayAuth?: "enforced" | "not-enforced" | "missing" | "maas";
  /** The model's in-cluster address. */
  internalUrl?: string;
  /** The name the model answers to: the "model" a request sends. */
  servedName?: string;
  /** spec.model.name has characters that are not safe in commands, so servedName is left out and no command is shown. */
  servedNameWithheld?: boolean;
  /** The last "Can it be called?" check since the app started. */
  callCheck?: ModelCallCheck;
}

/** What a model setup needs, and the catalog's models in the projects. */
export interface ModelsStatus {
  /** Why no model can be set up (KServe off, LLM dependencies missing). */
  blocked?: ComponentBlock | null;
  /** DataScienceCluster KserveLLMInferenceServiceDependencies; absent: not reported (unknown). */
  dependencies?: "True" | "False";
  /** The CPU preset found at run time, or why none was found. */
  preset?: string;
  presetProblem?: string;
  /** The image a new model would run; absent: unknown. */
  imagePlan?: "preset" | "released" | "override";
  imagePlanReason?: string;
  catalog: CatalogModel[] | null;
  items?: ModelState[] | null;
}

/** The body of POST /api/resources/models/stream (pkg/types ModelsRequest). */
export interface ModelsRequest {
  /** allow / disallow: add project to (take it off) the projects this tool's inference gateway allows; check: call the model once. */
  action: "setup" | "teardown" | "allow" | "disallow" | "check";
  /** Setup: how the model is reached (absent: the recommended way). */
  exposure?: ModelExposureKind;
  /** The gateway of exposure "gateway": "namespace/name". */
  gatewayRef?: string;
  /** The inference gateway: set up before the model, deleted after it. */
  gateway?: boolean;
  /** The model's project (absent: no model part), or the project allow adds. */
  project?: string;
  model?: string;
  /** Setup: run the newest released Red Hat image instead of the preset's. */
  releasedImage?: boolean;
}

export interface KeptPVC {
  name: string;
  size?: string;
}

// --- GET /api/operation (pkg/api OperationStatus) ---

/** The cluster operation that holds the backend's mutation lock. */
export interface ServerOperation {
  id: string;
  /** update, reinstall, refresh, deploy-dashboard-pr, setup-s3, ... */
  type: string;
  label: string;
  user: string;
  target?: string;
  /** RFC 3339. */
  startedAt: string;
  updatedAt?: string;
  /** Step id of the latest progress event (streamed operations only). */
  step?: string;
  stepStatus?: string;
  message?: string;
  pod?: string;
  /**
   * The operation runs in another updater pod (for example one that is
   * shutting down and finishes its operation first). The backend knows it
   * from that pod's lease: step and message are as of its last heartbeat,
   * and there is no stream to attach to.
   */
  remote?: boolean;
}

/** An operation an updater pod started and never finished (its pod stopped). */
export interface InterruptedOperation {
  type: string;
  label: string;
  user: string;
  target?: string;
  startedAt: string;
  pod?: string;
  /** The operation's ID (pkg/types OperationMarker). */
  id?: string;
  /** The server process that wrote the marker: a restarted container keeps its pod name but not this. */
  bootId?: string;
}

/**
 * The most recently finished operation (GET /api/operation `lastCompleted`).
 * Older backends do not send it; treat its absence as "outcome unknown".
 */
export interface CompletedOperation {
  id: string;
  type: string;
  label: string;
  user: string;
  target?: string;
  /** RFC 3339. */
  startedAt: string;
  finishedAt?: string;
  success: boolean;
  message?: string;
}

export interface OperationStatusResponse {
  inProgress: boolean;
  operation: ServerOperation | null;
  interrupted?: InterruptedOperation;
  lastCompleted?: CompletedOperation | null;
}

/** GET /api/version */
export interface VersionInfo {
  version: string;
  commit: string;
  buildDate: string;
  goVersion?: string;
  templateRevision?: string;
  expectedTemplateRevision?: string;
  /** The Deployment predates the template this build expects (run make upgrade). */
  templateOutdated: boolean;
  /** "gitops": Argo CD installed it from the Helm chart, so upgrades are made in Git. Absent for install.sh and make. */
  installMethod?: "gitops";
  /** What the updater may do here (PROFILE). "gitops": only Update and the Status, Components and Diagnostics reads; absent from older backends (full). */
  profile?: "full" | "gitops";
  /** gitops profile: "enforced" when its admission policy was found at the last check, else why Update is off; absent before the first check. */
  admissionPolicy?: string;
}

/** GET /api/update-check: the newest release in the updater's image repository. */
export interface UpdateCheck {
  /** The running release; absent for main and commit builds (no check). */
  current?: string;
  latest?: string;
  updateAvailable: boolean;
  /** latest has another MAJOR version: it needs its own deploy template (a full redeploy). */
  majorUpgrade: boolean;
  releaseNotesURL?: string;
  /** Where latest's install.sh is downloaded from, when the releases host is known. */
  installerURL?: string;
  /** That install.sh's SHA-256 as latest's release notes publish it, when the backend could read them. */
  installerSHA256?: string;
  /** The gitops profile: no registry check; the chart pins the release in Git. */
  pinnedInGit?: boolean;
}

/** One build's answer in GET /api/build-explorer/contains. */
export interface PRContainsBuild {
  image: string;
  tag?: string;
  installed?: boolean;
  /** The component image's vcs-ref, and "owner/name" from its git.url label. */
  commit?: string;
  commitRepo?: string;
  result: "contains" | "not_contained" | "unknown";
  /**
   * Why: for not_contained, built_before_merge or built_before_commit (the
   * build is older than the PR's merge or the commit, so it was not read);
   * for unknown, rate_limited, commit_not_found (GitHub does not know the
   * merge commit or commit), no_component_image, no_commit_label, unexpected_repo,
   * build_unreadable or github_error.
   */
  reason?: string;
  message?: string;
  compareURL?: string;
}

/**
 * GET /api/build-explorer/contains?pr=N|commit=SHA[&repo=owner/name][&image=...]:
 * a PR search fills pr, title, url, mergeCommit and mergedAt; a commit search
 * fills commit, commitDate, commitTitle and commitURL.
 */
export interface StableBranch {
  branch: string;
  head: string;
  updatedAt?: string;
  /** stable has the PR's merge commit, or the commit. */
  contains: boolean;
}

export interface PRContainsResponse {
  repo: string;
  pr?: number;
  title?: string;
  url?: string;
  mergeCommit?: string;
  mergedAt?: string;
  commit?: string;
  commitDate?: string;
  commitTitle?: string;
  commitURL?: string;
  builds: PRContainsBuild[];
  /** Upstream odh-dashboard's stable branch, the step between main and the nightlies, when nothing contains the PR or commit. */
  stable?: StableBranch;
  rateLimited?: boolean;
  retryAfterSeconds?: number;
}

/** The Gen AI playground feature (GET /api/resources/playground; pkg/types PlaygroundStatus). */
export interface PlaygroundStatus {
  /** Why no playground can be set up (its operator's component is off). */
  blocked?: ComponentBlock;
  /** The DataScienceCluster's readiness condition of that component: "True", "False", or absent (unknown). */
  operatorReady?: string;
  operatorMessage?: string;
  /** The dashboard's Gen AI studio backend: "available", "unavailable", "missing", or absent (unknown). */
  genAiUi?: string;
  /** Why the dashboard's gen-ai API cannot be used now. */
  apiProblem?: string;
  apiCode?: string;
  /** The playgrounds in the data science projects, and the projects this tool requested one in. */
  items?: PlaygroundState[];
  /** The project the models are listed for. */
  project?: string;
  models?: PlaygroundModel[];
  modelsProblem?: string;
  truncated?: boolean;
}

/** A project's playground (namespace); managedByTool when this tool requested it. */
export interface PlaygroundState extends ResourceState {
  phase?: string;
  requested?: boolean;
}

/** A model the dashboard offers the playground. */
export interface PlaygroundModel {
  name: string;
  displayName?: string;
  /** "namespace" (deployed in the project) or "maas" (Models as a Service). */
  source: "namespace" | "maas";
  status?: string;
  ready: boolean;
  /** False for an embedding model. */
  chat: boolean;
  /** Set for this tool's embedding model: Set up adds it as an embedding model of this dimension. */
  embeddingDimension?: number;
  /** A MaaS model's subscriptions: a playground chat request for it names one. */
  subscriptions?: string[];
}

export interface PlaygroundModelRef {
  name: string;
  source: "namespace" | "maas";
}

/** The body of POST /api/resources/playground/stream. */
export interface PlaygroundRequest {
  action: "setup" | "teardown";
  project: string;
  models?: PlaygroundModelRef[];
}

/** One part of the MaaS platform (pkg/types MaaSPart). */
export interface MaaSPart {
  id: "rhcl" | "kuadrant" | "cnpg" | "database" | "gateway" | "authorino-tls" | "tier-policies" | "dsc" | "maas-api" | "tenant";
  title: string;
  /** "missing": Set up does it; "manual": a cluster-admin does it (commands says how). */
  state: "ready" | "missing" | "waiting" | "problem" | "manual" | "unknown";
  message?: string;
  managedByTool?: boolean;
  notManaged?: boolean;
  warning?: string;
  /** The same change from a terminal, for an admin who prefers it. */
  commands?: string;
  /** Fixes the part offers on its own. */
  actions?: MaaSAction[];
  docUrl?: string;
}

/** The fixes a part offers besides Set up. */
export type MaaSActionId = "upgrade-rhcl" | "rhcl-memory" | "rhcl-deny-on-failure" | "authorino-tls" | "remove-tier-policies" | "change-database";

/** A part's own fix (pkg/types MaaSAction): its confirmation lists changes. */
export interface MaaSAction {
  id: MaaSActionId;
  label: string;
  summary: string;
  changes: string[];
  danger?: boolean;
}

/** One of this tool's models and its MaaS publication (pkg/types MaaSModel). */
export interface MaaSModel extends ResourceState {
  displayName?: string;
  modelReady: boolean;
  published: boolean;
  /** Set up on the Models tab "Through MaaS only": Unpublish leaves it with no route (only inside the cluster). */
  maasOnlySetup?: boolean;
  /** What is left of a publication undone half-way (message says what); Unpublish finishes it. */
  partly?: boolean;
  phase?: string;
  endpoint?: string;
  access?: string;
  subscription?: string;
  /** Its entry in this tool's subscription (published only). */
  limit?: MaaSLimit;
}

// --- MaaS: the database choice and the subscription's settings (4.0.0) ---

/** Where MaaS keeps its API keys: the user's PostgreSQL, the docs' development database, or CloudNativePG (this tool's option). */
export type MaaSDatabaseKind = "external" | "development" | "cnpg";

/** The database Set up creates, or Change database switches to (pkg/types MaaSDatabaseRequest). The URL is never sent back. */
export interface MaaSDatabaseRequest {
  kind: MaaSDatabaseKind;
  connectionUrl?: string;
}

/** One database Set up can make, with every change it makes (pkg/types MaaSDatabaseChoice). */
export interface MaaSDatabaseChoice {
  id: MaaSDatabaseKind;
  plan: string[];
  blockedReason?: string;
}

/** The database (pkg/types MaaSDatabase). current "": none yet; "adopted": a maas-db-config this tool did not write. */
export interface MaaSDatabase {
  current: "" | "adopted" | MaaSDatabaseKind;
  namespace: string;
  needed?: boolean;
  default: MaaSDatabaseKind;
  choices: MaaSDatabaseChoice[];
  changeBlockedReason?: string;
}

/** Who may use this tool's published models (pkg/types MaaSAudience). */
export interface MaaSAudience {
  everyone: boolean;
  groups?: string[];
  users?: string[];
}

/** One tokenRateLimits entry: limit tokens per window ("1m", "24h"). */
export interface MaaSTokenRate {
  limit: number;
  window: string;
}

/** A published model's limit (pkg/types MaaSLimit). */
export interface MaaSLimit {
  preset: "free" | "premium" | "unlimited" | "custom";
  unlimited?: boolean;
  rates?: MaaSTokenRate[];
}

/** GET /api/resources/maas (pkg/types MaaSStatus). */
export interface MaaSStatus {
  blocked?: ComponentBlock;
  platform: ResourceState;
  parts: MaaSPart[];
  url?: string;
  subscription: string;
  dashboardUrl?: string;
  /** Every change Set up would make now, in order; absent when it has nothing to do. */
  setupPlan?: string[];
  items?: MaaSModel[];
  /** The database choice; absent from older servers. */
  database?: MaaSDatabase;
  /** The audience of this tool's subscription; absent while it does not exist. */
  access?: MaaSAudience & { groupProblems?: MaaSGroupProblem[] };
  /** The cluster's MaaSSubscription API has no unlimited model reference (read from its CRD). */
  noUnlimited?: boolean;
}

/** A group with no OpenShift Group (missing), or one that could not be checked (pkg/types MaaSGroupProblem). */
export interface MaaSGroupProblem {
  name: string;
  missing?: boolean;
  message: string;
  /** Create the Group and add a user; absent when the name cannot be an OpenShift Group's. */
  commands?: string;
}

/** The body of POST /api/resources/maas/stream. */
export interface MaaSRequest {
  action: "setup" | "teardown" | "publish" | "unpublish" | "change-limit" | "change-access" | MaaSActionId;
  project?: string;
  /** The catalog name of the chat model to publish or unpublish; absent: the default. */
  model?: string;
  /** setup (when needed) and change-database. */
  database?: MaaSDatabaseRequest;
  /** publish (first model) and change-access. */
  access?: MaaSAudience;
  /** publish and change-limit. */
  limit?: MaaSLimit;
  /** change-database: every API key is lost. */
  confirmKeysLost?: boolean;
}

// --- Models routing (pkg/types ModelExposures, ModelCallCheck) ---------------

/** How a model is reached. */
export type ModelExposureKind = "default-gateway" | "maas" | "gateway" | "internal";

/** One way to reach a model (pkg/types ModelExposureOption). */
export interface ModelExposureOption {
  /** "default-gateway", "maas", "internal" or "gateway:<namespace>/<name>". */
  key: string;
  exposure: ModelExposureKind;
  gatewayRef?: string;
  title: string;
  description: string;
  available: boolean;
  reason?: string;
  /** Where an unavailable option is made available: set up the inference gateway (Models tab), or MaaS (its tab). */
  fix?: "inference-gateway" | "maas-tab";
  /** Connectivity Link is not set up yet: Set up installs it first. */
  needsAuth?: boolean;
  warning?: string;
  changes?: string[];
  address?: string;
}

/** GET /api/resources/models/exposures (pkg/types ModelExposures). */
export interface ModelExposures {
  project: string;
  model: string;
  /** The recommended option's key ("" when none fits). */
  default: string;
  defaultReason?: string;
  options: ModelExposureOption[];
  kserveDefaultGateway?: string;
  authReady: boolean;
  recommendGateway?: boolean;
  recommendGatewayReason?: string;
}

/** One "Can it be called?" check (pkg/types ModelCallCheck). */
export interface ModelCallCheck {
  result: "ok" | "model-name" | "route" | "auth" | "forbidden" | "server-error" | "unreachable" | "unauthenticated" | "maas" | "not-checked" | "unexpected";
  ok: boolean;
  status?: number;
  message: string;
  url?: string;
  at: string;
}

// --- One-click prerequisites (pkg/types PrerequisitesStatus) -----------------

/** A DataScienceCluster component this tool turned on, and what it was. */
export interface RestorableComponent {
  component: string;
  previous?: string;
  feature: string;
}

/** A prerequisite part, as a MaaS part (ids "dsc-<component>", operator ids, "kuadrant", "dependencies"). */
export type PrerequisitePart = Omit<MaaSPart, "id"> & { id: string };

/** One feature's cluster prerequisites. */
export interface FeaturePrerequisites {
  ready: boolean;
  /** The parts in setup order. */
  parts: PrerequisitePart[];
  setupPlan?: string[];
  setupBlockedReason?: string;
  restorable?: RestorableComponent[];
}

/** The features the prerequisites cover. */
export type PrerequisiteFeature = "pipelines" | "mlflow" | "models" | "playground";

/** GET /api/resources/prerequisites. */
export interface PrerequisitesStatus {
  features: Partial<Record<PrerequisiteFeature, FeaturePrerequisites>>;
  /** The way to reach a model Models was planned for ("" none). */
  modelsExposure?: string;
}

/** The body of POST /api/resources/prerequisites/stream. */
export interface PrerequisitesRequest {
  feature: PrerequisiteFeature;
  exposure?: ModelExposureKind;
}
