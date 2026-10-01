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
  installPlan?: InstallPlanInfo;
  catalogPod?: CatalogPodInfo;
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
  logs: string[];
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
  tags: NightlyTag[];
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
  invalidFields: string[];
  missingComponents: string[];
  extraComponents?: string[];
  validationError?: string;
  defaultsError?: string;
}

export interface ComponentsResponse {
  dscCompatibility?: DSCCompatibility;
  components: ComponentInfo[];
  deployments: DeploymentInfo[];
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
  relatedImages: RelatedImage[];
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
  commitDate?: string;
  buildDate?: string;
  version?: string;
}

export interface DebugResponse {
  operatorPods: PodInfo[];
  applicationPods: PodInfo[];
  marketplacePods: PodInfo[];
  warnings?: string[];
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
  devImages?: { deployment: string; container: string; envVar: string; repository: string; currentImage: string; defaultImage: string; ready: boolean }[];
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

export interface ResourcesStatus {
  minio: ResourceState;
  mlflow: ResourceState;
  pipelineServers: ResourceState[];
}

export interface ResourceState {
  deployed: boolean;
  ready: boolean;
  message?: string;
  namespace?: string;
  apiRoute?: string;
  uiRoute?: string;
  currentImage?: string;
}

export interface UpdateStep {
  step: string;
  status: 'running' | 'success' | 'failed' | 'skipped';
  message: string;
  detail?: string;
  elapsedMs: number;
  errorCode?: string;
}

export interface InstallPlanInfo {
  name: string;
  phase: string;
  approved: boolean;
}

export interface CatalogPodInfo {
  name: string;
  phase: string;
  ready: boolean;
  restartCount: number;
}

// --- Diagnostics types (match backend cluster.DiagnosticResult JSON) ---

export interface DiagnosticResult {
  problems: Problem[];
  checks: CheckResult[];
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
