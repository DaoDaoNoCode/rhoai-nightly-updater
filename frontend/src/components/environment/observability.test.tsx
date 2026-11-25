import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import type { ObservabilityStatus } from "../../types";
import { ObservabilityTab, observabilityConfirm, observabilitySteps } from "./ObservabilityTab";
import { observabilityState } from "./featureState";

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
  configured: true, managedByTool: true, ready: true, namespace: "redhat-ods-monitoring", dashboardUrl: "https://rh-ai.apps.example.com",
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
