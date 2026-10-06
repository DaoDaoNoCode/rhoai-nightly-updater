import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ReconciliationProgress } from "./ReconciliationProgress";
import { OperationProgress } from "./OperationProgress";
import { ActivityLog } from "./ActivityLog";
import type { Problem } from "../types";
import { nightlyStatus, renderWithApp } from "../test/providers";
import { jsonResponse } from "../test/utils";
import { compareTagToInstalled, sentence, shortTarget } from "../build";
import { describeError } from "../errors";

function stubDiagnostics(problems: Problem[], fixResponse: unknown, fixStatus = 200) {
  const fixBodies: unknown[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/diagnostics") return jsonResponse({ problems, checks: [] });
    if (url === "/api/diagnostics/fix") {
      fixBodies.push(JSON.parse(String(init?.body)));
      return jsonResponse(fixResponse, fixStatus);
    }
    if (url === "/api/assist-rollout") throw new Error("assist-rollout must not be called directly");
    return jsonResponse({});
  }));
  return fixBodies;
}

const renderStuck = () => renderWithApp(
  <ReconciliationProgress status={nightlyStatus({ csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Installing" } })} active startTime={Date.now() - 600_000} timedOut operationType="update" />,
);

describe("ReconciliationProgress stuck guidance (B2 contract)", () => {
  it("sends every fix, including assist-rollout:<ns>/<name>, to /api/diagnostics/fix and shows nothing_to_do as information", async () => {
    const fixBodies = stubDiagnostics([{
      id: "pod-unschedulable-redhat-ods-applications-foo", severity: "critical", title: "foo: 1 pod can't be scheduled", description: "Insufficient cpu",
      evidence: ["0/3 nodes are available: 3 Insufficient cpu."], autoFixable: true, autoFixAction: "assist-rollout:redhat-ods-applications/foo",
      fix: "Let the rollout replace one old pod at a time", affectedObjects: ["Deployment redhat-ods-applications/foo"],
    }], { success: false, errorCode: "nothing_to_do", message: "Nothing to do: the rollout is no longer blocked", logs: [] });
    renderStuck();
    expect(await screen.findByText("0/3 nodes are available: 3 Insufficient cpu.")).toBeInTheDocument();
    const apply = screen.getByRole("button", { name: /Apply fix/ });
    await waitFor(() => expect(apply).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(apply);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Deployment redhat-ods-applications/foo")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply fix" }));
    await waitFor(() => expect(fixBodies).toEqual([{ problemId: "assist-rollout:redhat-ods-applications/foo" }]));
    const result = await screen.findByText("Nothing to do: the rollout is no longer blocked");
    expect(result.closest(".pf-v6-c-alert")).toHaveClass("pf-m-info");
  });

  it("a cluster_busy refusal is a 'Cluster busy' warning, and the problems are scanned again (N8)", async () => {
    let scans = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url === "/api/diagnostics") {
        scans++;
        return jsonResponse({ problems: [{
          id: "webhook-stale", severity: "warning", title: "1 webhook configuration points to a missing Service", description: "Leftover webhook.",
          fix: "Delete it.", autoFixable: true, autoFixAction: "delete-stale-webhooks",
        }], checks: [] });
      }
      if (url === "/api/diagnostics/fix") return jsonResponse({ error: "alice is running \"Update to nightly\". Wait for it to finish.", errorCode: "cluster_busy" }, 409);
      return jsonResponse({});
    }));
    renderStuck();
    const apply = await screen.findByRole("button", { name: /Apply fix/ });
    await waitFor(() => expect(apply).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(apply);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Apply fix" }));
    const title = await screen.findByText("Cluster busy");
    expect(title.closest(".pf-v6-c-alert")).toHaveClass("pf-m-warning");
    await waitFor(() => expect(scans).toBe(2));
  });

  it("shows guidance-only problems with instructions and no fix button", async () => {
    stubDiagnostics([{
      id: "pod-stuck-creating-redhat-ods-applications-odh-observability", severity: "warning", title: "odh-observability: 2 pods stuck in ContainerCreating",
      description: "secret \"odh-observability-webhook-cert\" not found", autoFixable: false, fix: "The operator creates this secret; check the operator logs.",
    }], {});
    renderStuck();
    expect(await screen.findByText(/stuck in ContainerCreating/)).toBeInTheDocument();
    expect(screen.getByText("What to do:")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Apply fix/ })).not.toBeInTheDocument();
  });
});

describe("Reconcile completion after a reload (A07-8)", () => {
  beforeEach(() => {
    sessionStorage.setItem("rhoai-reconciling", "true");
    sessionStorage.setItem("rhoai-reconcile-start", String(Date.now() - 60_000));
    sessionStorage.setItem("rhoai-reconcile-kind", "refresh");
  });
  afterEach(() => sessionStorage.clear());

  it("names the operation that finished, shows the build, and can be dismissed", async () => {
    // The run succeeded before the reload (the stream said so).
    sessionStorage.setItem("rhoai-reconcile-meta", JSON.stringify({ result: "succeeded", serverId: "op-1" }));
    renderWithApp(<OperationProgress />);
    expect(await screen.findByText("Re-deploy complete")).toBeInTheDocument();
    expect(screen.getByText(/Operator rhods-operator.3.6.0 is Succeeded/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Close/ }));
    await waitFor(() => expect(screen.queryByText("Re-deploy complete")).not.toBeInTheDocument());
  });

  it("does not claim success when the result was never seen (N4), unless the server reports it", async () => {
    const view = renderWithApp(<OperationProgress />);
    expect(await screen.findByText("Re-deploy finished (outcome unknown, see the activity log)")).toBeInTheDocument();
    expect(screen.queryByText("Re-deploy complete")).not.toBeInTheDocument();
    view.unmount();

    sessionStorage.setItem("rhoai-reconciling", "true");
    sessionStorage.setItem("rhoai-reconcile-start", String(Date.now() - 60_000));
    sessionStorage.setItem("rhoai-reconcile-kind", "refresh");
    sessionStorage.setItem("rhoai-reconcile-meta", JSON.stringify({ serverId: "op-7" }));
    renderWithApp(<OperationProgress />, {
      operation: async () => ({ inProgress: false, operation: null, lastCompleted: { id: "op-7", type: "refresh", label: "Re-deploy", user: "me", startedAt: "2026-10-05T10:00:00Z", success: true } }),
    });
    expect(await screen.findByText("Re-deploy complete")).toBeInTheDocument();
  });
});

describe("ActivityLog (A07-13)", () => {
  const entries = [
    { timestamp: "2026-10-01T10:00:00Z", user: "alice", action: "update", detail: "x", success: true, label: "Updated to nightly", category: "operator", build: "rhoai-3.6 · 4eff06d60bd1" },
    { timestamp: "2026-10-02T10:00:00Z", user: "bob", action: "deploy-dashboard-pr", detail: "PR #1", success: true, label: "Dashboard PR deployed", category: "dashboard-dev" },
    { timestamp: "2026-10-03T10:00:00Z", user: "bob", action: "revert-dashboard", detail: "", success: false, label: "Dashboard reverted", category: "dashboard-dev" },
  ];

  it("shows operator changes with their build first and filters by category", () => {
    renderWithApp(<ActivityLog activity={entries} />);
    expect(screen.getByText("Updated to nightly")).toBeInTheDocument();
    // Tag and digest are separate nowrap items (no separator glyph that could start or end a line).
    const build = screen.getByText("4eff06d60bd1").closest(".pf-v6-l-flex") as HTMLElement;
    expect(build).toHaveTextContent(/^rhoai-3\.64eff06d60bd1$/);
    expect(screen.queryByText("Dashboard PR deployed")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Activity type: Operator" }));
    fireEvent.click(screen.getByRole("option", { name: /Dashboard Dev\s*2/ }));
    expect(screen.getByText("Dashboard PR deployed")).toBeInTheDocument();
    expect(screen.getByText("(failed)")).toBeInTheDocument();
  });

  it("falls back to readable labels for entries without backend labels", () => {
    renderWithApp(<ActivityLog activity={[{ timestamp: "2026-10-01T10:00:00Z", user: "a", action: "repair-dsc", detail: "", success: true }]} defaultCategory="all" />);
    expect(screen.getByText("Repair dsc")).toBeInTheDocument();
  });
});

describe("helpers", () => {
  it("compares a nightly tag's release line with the installed version", () => {
    expect(compareTagToInstalled("rhoai-3.5", "3.6.0")).toBe("older");
    expect(compareTagToInstalled("rhoai-3.6-ea.2", "3.6.0")).toBe("same-line");
    expect(compareTagToInstalled("rhoai-3.7", "3.6.0")).toBe("newer");
    expect(compareTagToInstalled("custom", "3.6.0")).toBe("unknown");
  });

  it("shortens image targets and ends messages with a period before a hint", () => {
    expect(shortTarget("nightly quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64))).toBe("nightly rhoai-3.6 · aaaaaaaaaaaa");
    expect(shortTarget("stable")).toBe("stable");
    expect(sentence("dial tcp: i/o timeout")).toBe("dial tcp: i/o timeout.");
    // A message that ends with a parenthesis still needs its full stop.
    expect(sentence("CSV rhods-operator.3.6.0: Installing (InstallWaiting)")).toBe("CSV rhods-operator.3.6.0: Installing (InstallWaiting).");
    expect(sentence("Restarted (see the log.)")).toBe("Restarted (see the log.)");
    expect(sentence('It said "done."')).toBe('It said "done."');
    expect(sentence("Ready:")).toBe("Ready:");
    const d = describeError({ name: "ApiError", status: 502, errorCode: "network", message: "dial tcp 1.2.3.4:443: i/o timeout" });
    expect(d.title).toBe("Cannot reach the OpenShift API");
    expect(d.body).toBe("dial tcp 1.2.3.4:443: i/o timeout.");
    expect(d.hint).toMatch(/Retry/);
  });
});
