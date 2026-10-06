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
  stableSource: string;
  stableChannel: string;
  stableVersion?: string;
  stableChannelPinned?: boolean;
  stableDiscoveryError?: string;
  activity?: ActivityEntry[];
  errors?: string[];
  dscExists: boolean;
  /** Installed vs latest nightly; only while the Subscription uses the nightly catalog. */
  nightly?: NightlyStatus;
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
}

export interface SubscriptionInfo {
  name: string;
  source: string;
  channel: string;
  state: string;
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
}

export interface PullSecretInfo {
  exists: boolean;
  valid: boolean;
  detail?: string;
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
  /** Present whenever dashboard-operator exists; `active` while it is paused. */
  override?: DashboardOverride;
}

/** A paused dashboard-operator / Dashboard Dev session (pkg/types DashboardOverride). */
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
  minio: ResourceState;
  mlflow: ResourceState;
  pipelineServers: ResourceState[] | null;
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
  /** A notice that needs action but does not stop the resource (another S3 storage image, MinIO not yet migrated, exposed S3 Route). */
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
  /** Enabled components "reset-defaults" would set to Removed or drop (FXA). */
  resetRemovals?: string[] | null;
  /** Components a repair would remove that must not be removed now, with why (FXA). */
  removalBlocks?: DSCRemovalBlock[] | null;
}

export interface DSCRemovalBlock {
  component: string;
  reasons: string[];
}

export interface ComponentsResponse {
  dscExists?: boolean;
  /** "present", "no-dsc" (CRD installed, no DSC) or "no-crd" (no DSC API). */
  dscState?: string;
  operatorVersion?: string;
  operatorPhase?: string;
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

/** The Dashboard Dev session recorded on dashboard-operator (pkg/types DashboardOverride). */
export interface DashboardOverride {
  active: boolean;
  operatorPaused: boolean;
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

export type DashboardFlavor = "rhoai" | "odh";

export interface DashboardState {
  override?: DashboardOverride;
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

export interface ResourcesStatus {
  /** Projects that already have a pipeline server the tool did not create. */
  unmanagedPipelineProjects?: string[] | null;
}

export interface ResourceState {
  /** DSPA name for pipeline servers ("nightly-dspa", or "dspa" for legacy ones). */
  name?: string;
  /** False: the tool did not create it and will not tear it down. */
  managedByTool?: boolean;
  terminating?: boolean;
  setupBlockedReason?: string;
  teardownBlockedReason?: string;
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
  /** The S3 storage still runs the MinIO of an earlier version; setup replaces it and starts fresh. */
  migrationPending?: boolean;
  /** Data volumes kept on purpose after the migration (the old MinIO PVC); teardown deletes them. */
  keptPVCs?: KeptPVC[] | null;
  /**
   * What re-running setup (Repair) would fix: the server does not serve
   * through its Service or Route, is scaled to zero, or a migration left
   * cleanup undone. Not ready: incomplete; ready: degraded.
   */
  repairNeeded?: string;
}

export interface KeptPVC {
  name: string;
  size?: string;
}

// --- GET /api/operation (pkg/api OperationStatus) ---

/** The cluster operation that holds the backend's mutation lock. */
export interface ServerOperation {
  id: string;
  /** update, reinstall, refresh, deploy-dashboard-pr, setup-minio, ... */
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
  /** Why the result is unknown: rate_limited, commit_not_found, no_component_image, no_commit_label, unexpected_repo, build_unreadable, github_error. */
  reason?: string;
  message?: string;
  compareURL?: string;
}

/** GET /api/build-explorer/contains?pr=N[&image=...] */
export interface PRContainsResponse {
  repo: string;
  pr: number;
  title?: string;
  url?: string;
  mergeCommit?: string;
  mergedAt?: string;
  builds: PRContainsBuild[];
  rateLimited?: boolean;
  retryAfterSeconds?: number;
}
