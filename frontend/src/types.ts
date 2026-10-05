// These types mirror the JSON of pkg/types/types.go (and pkg/cluster for
// diagnostics). A Go slice field without omitempty encodes nil as null, so
// such fields are typed `T[] | null` and read with `?? []`.

export interface ActivityEntry {
  timestamp: string;
  user: string;
  action: string;
  detail: string;
  success: boolean;
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
  /** Planned backend fields: the resource can't become ready without a change. */
  terminal?: boolean;
  /** Planned backend field: a container waiting reason such as ImagePullBackOff. */
  waitingReason?: string;
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
}

export interface CheckResult {
  name: string;
  status: "pass" | "fail" | "warn" | "info";
  detail: string;
}
