import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import type { ObservabilityStatus } from "../../types";
import { ObservabilityTab, observabilityConfirm, observabilitySteps } from "./ObservabilityTab";
import { observabilityState } from "./featureState";
import { MaaSTab, maasConfirm, maasRequestOf, maasSetupChoices, maasSteps } from "./MaaSTab";
import type { MaaSStatus } from "../../types";
import { IDLE } from "./featureState";
import { CleanupModal, cleanupBlocked, cleanupCandidates, cleanupSteps } from "./CleanupModal";
import type { ResourcesStatus } from "../../types";
import { contract } from "../../test/contract";

const fresh: ObservabilityStatus = {
  configured: false, managedByTool: false, ready: false, namespace: "redhat-ods-monitoring",
  parts: [
    { id: "loki-operator", title: "Loki Operator", state: "missing", message: "Not installed. Set up installs version 6.7.0 from redhat-operators (channel stable-6.7)." },
    { id: "usage-logs-storage", title: "Usage-logs storage", state: "missing", message: "Not set up." },
    { id: "monitoring-settings", title: "Monitoring settings (DSCInitialization)", state: "missing", message: "DSCInitialization default-dsci has no spec.monitoring.usageLogs." },
  ],
  setupPlan: ["Install Loki Operator 6.7.0", "DSCInitialization default-dsci: add spec.monitoring.usageLogs"],
};

const ready: ObservabilityStatus = {
  configured: true, managedByTool: true, ready: true, usesS3Storage: true, namespace: "redhat-ods-monitoring", dashboardUrl: "https://rh-ai.apps.example.com",
  parts: [{ id: "usage-logs", title: "Usage logs (LokiStack and collector)", state: "ready", message: "Ready." }],
  volumes: [
    { name: "storage-data-science-usage-logs-lokistack-ingester-0", size: "10Gi", storageClass: "gp3-csi", setUpByTool: true },
    { name: "someone-elses", size: "1Gi" },
  ],
  volumesTotal: "11Gi",
  cleanupPlan: ["DSCInitialization default-dsci: remove spec.monitoring.usageLogs"],
  cleanupKept: ["The operators: left in place"],
};

function renderTab(o: ObservabilityStatus | null, extra: Partial<React.ComponentProps<typeof ObservabilityTab>> = {}) {
  const onSetUp = vi.fn();
  const setPending = vi.fn();
  render(
    <MemoryRouter>
      <ObservabilityTab observability={o} observabilityError={null} onRetry={() => {}} state={observabilityState(o)} baseReason={null} readOnly={false}
        setPending={setPending} onSetUp={onSetUp} {...extra} />
    </MemoryRouter>,
  );
  return { onSetUp, setPending };
}

describe("Observability tab", () => {
  it("a fresh cluster: the parts, the cost and Set up", () => {
    const { onSetUp } = renderTab(fresh);
    expect(screen.getByRole("heading", { name: "Loki Operator" })).toBeInTheDocument();
    expect(screen.getByText(/^Nothing yet. Set up adds: About 51Gi of disks in 7 PersistentVolumeClaims/)).toBeInTheDocument();
    expect(screen.getAllByText(/not for production/).length).toBeGreaterThan(0);
    screen.getByRole("button", { name: "Set up usage logs and dashboards" }).click();
    expect(onSetUp).toHaveBeenCalledWith([{ kind: "setup-observability" }]);
    expect(screen.queryByRole("button", { name: "Clean up usage logs" })).toBeNull();
  });

  it("Set up says why it cannot run", () => {
    renderTab({ ...fresh, setupBlockedReason: "Usage logs get an S3 key of their own, which needs secured S3 storage." });
    expect(screen.getByRole("button", { name: "Set up usage logs and dashboards" })).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByText(/needs secured S3 storage/)).toBeInTheDocument();
  });

  it("ready: how to use it, the admin view's limit, the disks and Clean up", () => {
    const { setPending } = renderTab(ready);
    expect(screen.getByText(/shown to cluster-admins only/)).toBeInTheDocument();
    expect(screen.getByText(/stays empty until MaaS records user IDs/)).toBeInTheDocument();
    const disks = screen.getByRole("grid", { name: "Disks of RHOAI's monitoring" });
    expect(within(disks).getByText("Deleted with its data")).toBeInTheDocument();
    expect(within(disks).getByText("Kept")).toBeInTheDocument();
    screen.getByRole("button", { name: "Clean up usage logs" }).click();
    expect(setPending).toHaveBeenCalledWith({ kind: "teardown-observability" });
  });

  it("usage logs someone else set up: shown as they are, no Clean up", () => {
    renderTab({ ...ready, managedByTool: false, external: true, setupPlan: [] });
    expect(screen.getByText("Usage logs are set up on this cluster, not by this tool")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clean up usage logs" })).toBeNull();
  });

  it("the collector problem: the manual fix is labelled as outside the docs", () => {
    renderTab({ ...ready, ready: false, parts: [{ id: "metrics-collector", title: "Metrics collector (OpenTelemetry)", state: "problem", message: "This is a product bug.",
      commands: "# Not in RHOAI docs; worked in our test: restart the OpenTelemetry operator once (OLM makes its pod again).\noc delete pod -n openshift-opentelemetry-operator -l control-plane=controller-manager" }] });
    expect(screen.getByText(/oc delete pod -n openshift-opentelemetry-operator/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Copy the manual fix \(not in RHOAI docs; worked in our test\)/ })).toBeInTheDocument();
  });
});

describe("Observability state and confirmations", () => {
  it("the Overview's state", () => {
    expect(observabilityState(null).loading).toBe(true);
    expect(observabilityState(fresh)).toMatchObject({ status: "not-set-up", label: "Not set up", next: { kind: "set-up", label: "Set up usage logs" } });
    expect(observabilityState(ready)).toMatchObject({ status: "ready", next: { kind: "open", href: "https://rh-ai.apps.example.com" } });
    expect(observabilityState({ ...ready, ready: false, parts: [{ id: "metrics-collector", title: "x", state: "problem" }] })).toMatchObject({ status: "needs-attention", next: { kind: "show-issue", part: "observability-metrics-collector" } });
    expect(observabilityState({ ...ready, external: true })).toMatchObject({ status: "ready", unmanaged: true });
  });

  it("Clean up warns about the data it deletes", () => {
    const spec = observabilityConfirm({ kind: "teardown-observability" }, ready)!;
    expect(spec.danger).toBe(true);
    render(<>{spec.dataLoss}{spec.extra}</>);
    expect(screen.getByText(/1 disks \(10Gi, listed under Disks\)/)).toBeInTheDocument();
    expect(screen.getByText(/Kept: The operators: left in place/)).toBeInTheDocument();
    expect(spec.changes).toHaveLength(1);
  });

  it("the steps follow the backend's", () => {
    expect(observabilitySteps({ kind: "setup-observability" }).map((s) => s.id)).toEqual(["observability-check", "cert-manager", "cluster-observability-operator", "opentelemetry", "loki-operator", "usage-logs-storage", "monitoring-settings", "monitoring-ready"]);
    expect(observabilitySteps({ kind: "teardown-observability" }).map((s) => s.id)).toEqual(["observability-check", "monitoring-settings", "monitoring-gc", "monitoring-leftovers", "usage-logs-storage"]);
  });
});

const maasBase: MaaSStatus = {
  platform: { deployed: true, ready: true, managedByTool: true },
  parts: [{ id: "dsc", title: "MaaS in the DataScienceCluster", state: "ready" }],
  subscription: "rhoai-nightly-updater-subscription",
  usageLogs: { state: "missing", message: "Not set up.", tenantFound: true },
  portal: { on: false, managementState: "Removed", available: "False" },
};

describe("MaaS: usage logs, user IDs and the portal", () => {
  it("Set up's checkboxes: usage logs ticked when the S3 storage allows them, user IDs never by default", () => {
    expect(maasSetupChoices({ kind: "setup-maas" }, maasBase)).toEqual({ offer: true, usageLogs: true, captureUser: false, offerCapture: true });
    const blocked = { ...maasBase, usageLogs: { ...maasBase.usageLogs!, setUpBlockedReason: "needs secured S3 storage" } };
    expect(maasSetupChoices({ kind: "setup-maas", usageLogs: true }, blocked)).toMatchObject({ offer: true, usageLogs: false });
    expect(maasRequestOf({ kind: "setup-maas", captureUser: true }, maasBase)).toEqual({ action: "setup", usageLogs: true, captureUser: true });
    expect(maasRequestOf({ kind: "setup-maas", usageLogs: false }, maasBase)).toEqual({ action: "setup" });
    expect(maasRequestOf({ kind: "maas-portal", on: true }, maasBase)).toEqual({ action: "portal-on" });
    expect(maasRequestOf({ kind: "maas-capture-user", on: false }, maasBase)).toEqual({ action: "capture-user-off" });
  });

  it("Set up's steps: the usage logs first, user IDs last", () => {
    const ids = maasSteps({ kind: "setup-maas" }, { action: "setup", usageLogs: true, captureUser: true }).map((s) => s.id);
    expect(ids.slice(0, 2)).toEqual(["observability-check", "cert-manager"]);
    expect(ids.indexOf("monitoring-ready")).toBeLessThan(ids.indexOf("check"));
    expect(ids[ids.length - 1]).toBe("capture-user");
    expect(maasSteps({ kind: "setup-maas" }).map((s) => s.id)[0]).toBe("check");
  });

  it("the confirmation says what recording user IDs does", () => {
    const spec = maasConfirm({ kind: "setup-maas" }, maasBase, () => {})!;
    render(<>{spec.extra}</>);
    expect(screen.getByRole("checkbox", { name: "Also set up usage logs and dashboards (first)" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Record user IDs for per-user usage (My Usage)" })).not.toBeChecked();
    expect(screen.getByText(/shows nothing \(0 for every user\)/)).toBeInTheDocument();
    expect(screen.getByText(/OpenShift user name/)).toBeInTheDocument();
  });

  it("the rows: usage logs with Record user IDs, the portal with Turn on", () => {
    const setPending = vi.fn();
    render(
      <MemoryRouter>
        <MaaSTab maas={maasBase} maasError={null} onRetry={() => {}} state={{ status: "ready", label: "Ready", next: null, project: null }} busy={IDLE} baseReason={null} readOnly={false}
          setPending={setPending} onSetUp={() => {}} onModels={() => {}} onObservability={() => {}} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("heading", { name: "Usage logs and dashboards" })).toBeInTheDocument();
    expect(screen.getByText(/shown to cluster-admins only/)).toBeInTheDocument();
    screen.getByRole("button", { name: "Record user IDs" }).click();
    expect(setPending).toHaveBeenCalledWith({ kind: "maas-capture-user", on: true });
    screen.getByRole("button", { name: "Turn on" }).click();
    expect(setPending).toHaveBeenCalledWith({ kind: "maas-portal", on: true });
  });
});

describe("Clean up everything: the usage logs", () => {
  const status: ResourcesStatus = { minio: { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs" }, mlflow: { deployed: false, ready: false, managedByTool: false }, pipelineServers: [] };

  it("offers them only when this tool set them up, and runs them before the S3 storage", () => {
    expect(cleanupCandidates(status, "all", false, null, null, null, null, null, ready).observability).toBe(true);
    expect(cleanupCandidates(status, "all", false, null, null, null, null, null, { ...ready, managedByTool: false, external: true }).observability).toBe(false);
    expect(cleanupCandidates(status, "mlflow", false, null, null, null, null, null, ready).observability).toBe(false);
    const ids = cleanupSteps({ maasPublished: ["p"], storage: true, observability: true, maasPlatform: true }).map((s) => s.id);
    expect(ids).toEqual(["maas-unpublish:p", "observability", "storage", "maas-platform"]);
  });

  it("the S3 storage without them is refused at plan time, with what to do", () => {
    expect(cleanupBlocked(status, { storage: true }, [], true)).toMatch(/Select Usage logs and dashboards too, or keep the S3 storage/);
    expect(cleanupBlocked(status, { storage: true, observability: true }, [], true)).toBeNull();
    expect(cleanupBlocked(status, { storage: true }, [], false)).toBeNull();
  });

  it("the dialog ticks them, says what is lost, and disables Delete when they are unticked but the storage is not", async () => {
    render(<CleanupModal scope="all" status={status} observability={ready} minioLeftovers={false} baseReason={null} onConfirm={() => {}} onCancel={() => {}} />);
    const box = screen.getByRole("checkbox", { name: /Usage logs and dashboards/ });
    expect(box).toBeChecked();
    expect(screen.getByText(/Every usage log and metric/)).toBeInTheDocument();
    fireEvent.click(box);
    await waitFor(() => expect(screen.getAllByText(/keep their data in the S3 storage/).length).toBeGreaterThan(0));
  });

  it("the contract fixture fits the tab", () => {
    const o = contract.observability();
    expect(o.parts.length).toBeGreaterThan(0);
    expect(observabilityState({ ...o, blocked: undefined }).status).toBe("needs-attention");
  });
});

describe("Observability tab, read-only", () => {
  it("says it is for cluster-admins instead of loading forever", () => {
    render(
      <MemoryRouter>
        <ObservabilityTab observability={null} observabilityError={null} onRetry={() => {}} state={observabilityState(null)} baseReason={null} readOnly setPending={() => {}} onSetUp={() => {}} />
      </MemoryRouter>,
    );
    expect(screen.getByText("Shown to cluster-admins only")).toBeInTheDocument();
  });
});

describe("Clean up everything: the server says whether the usage logs use the S3 storage", () => {
  const status: ResourcesStatus = { minio: { deployed: true, ready: true, managedByTool: true, namespace: "seaweedfs" }, mlflow: { deployed: false, ready: false, managedByTool: false }, pipelineServers: [] };
  it("does not block the storage when the server says the usage logs are elsewhere", async () => {
    render(<CleanupModal scope="all" status={status} observability={{ ...ready, usesS3Storage: false }} minioLeftovers={false} baseReason={null} onConfirm={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole("checkbox", { name: /Usage logs and dashboards/ }));
    await waitFor(() => expect(screen.getByRole("checkbox", { name: /Usage logs and dashboards/ })).not.toBeChecked());
    expect(screen.queryByText(/keep their data in the S3 storage/)).toBeNull();
  });
});
