import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { TroubleshootingPage, buildDiagnosticReport, problemSummary } from "./TroubleshootingPage";
import { stubApi, jsonResponse } from "../test/apiStub";
import { renderPage } from "../test/providers";
import type { DiagnosticResult, OperationStatusResponse, Problem } from "../types";

// Shapes from the live GET /api/diagnostics (B2) on 2026-10-05.
const guidance: Problem = {
  id: "stale-crd-conversion", severity: "critical",
  title: "1 CRD(s) use a conversion webhook whose Service is missing",
  description: "Reading or writing these objects fails.",
  evidence: ["CRD mcpservers.mcp.x-k8s.io: conversion Service redhat-ods-applications/mcp-lifecycle-operator-webhook-service not found"],
  fix: "Bring back the Service that serves the conversion.",
  autoFixable: false,
  affectedObjects: ["CustomResourceDefinition mcpservers.mcp.x-k8s.io"],
  technicalCmd: "oc get crd -o custom-columns=NAME:.metadata.name,STRATEGY:.spec.conversion.strategy",
};
const pods: Problem = {
  id: "pod-stuck-creating-redhat-ods-applications-odh-observability", severity: "warning",
  title: 'odh-observability: 2 pods stuck in ContainerCreating: secret "odh-observability-webhook-cert" not found',
  description: "The pods are scheduled on a node, but the containers have not started for 2d.",
  evidence: ['FailedMount: MountVolume.SetUp failed for volume "webhook-certs" : secret "odh-observability-webhook-cert" not found'],
  fix: "The pod mounts an object that does not exist yet.", autoFixable: false,
  technicalCmd: "oc describe pod odh-observability-7b6d65d9b7-vxzxh -n redhat-ods-applications",
};
const fixable: Problem = {
  id: "webhook-stale", severity: "warning", title: "1 webhook configuration points to a missing Service",
  description: "Leftover webhook.", fix: "Delete it.", autoFixable: true, autoFixAction: "delete-stale-webhooks",
  confirmMessage: "Delete the webhook configurations listed below.",
  affectedObjects: ["ValidatingWebhookConfiguration datasciencecluster-v2-validator.opendatahub.io-nfzvz"],
};

function result(problems: Problem[]): DiagnosticResult {
  return {
    problems,
    checks: [
      { name: "Catalog health", status: "pass", detail: "Nightly catalog is healthy" },
      { name: "DataScienceCluster", status: "fail", detail: "default-dsc is not ready" },
      { name: "Platform modules", status: "info", detail: "no module is stuck in deletion" },
    ],
  };
}


/** Problems start collapsed (UX-Diagnostics-3): open one by its card toggle. */
function expand(title: string): void {
  const card = screen.getByText(title).closest(".pf-v6-c-card") as HTMLElement;
  fireEvent.click(within(card).getByRole("button", { name: /Details/ }));
}

describe("Diagnostics (B2 contract)", () => {
  it("guidance-only problems show the objects and a copyable command, and no Fix button", async () => {
    stubApi({ "/api/diagnostics": result([guidance, pods]) });
    renderPage(<TroubleshootingPage />);
    const card = (await screen.findByText(guidance.title)).closest(".pf-v6-c-card") as HTMLElement;
    expect(within(card).queryByText("Auto-fix available")).not.toBeInTheDocument();
    expect(within(card).queryByRole("textbox")).not.toBeInTheDocument();
    expand(guidance.title);
    expect(within(card).getByText(/This page does not change anything for this problem/)).toBeInTheDocument();
    expect(within(card).getByText("CustomResourceDefinition mcpservers.mcp.x-k8s.io")).toBeInTheDocument();
    expect(within(card).getByRole("textbox", { name: `Command for ${guidance.title}` })).toHaveValue(guidance.technicalCmd);
    expect(within(card).getByRole("button", { name: "Copy command" })).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Fix" })).not.toBeInTheDocument();
    // A08-2: the pod's real cause is in the title and evidence.
    expect(screen.getByText(pods.title)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Fix" })).not.toBeInTheDocument();
    expect(screen.getByText("Info")).toBeInTheDocument();
  });

  it("the fix confirmation lists the exact objects; nothing_to_do is info and the page re-scans", async () => {
    const api = stubApi({
      "/api/diagnostics": result([fixable]),
      "POST /api/diagnostics/fix": () => jsonResponse({ success: false, errorCode: "nothing_to_do", message: "Nothing to do: every Service exists.", logs: [] }, 200),
    });
    renderPage(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    expand(fixable.title);
    // Wait for permissions, then open the dialog.
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("ValidatingWebhookConfiguration datasciencecluster-v2-validator.opendatahub.io-nfzvz")).toBeInTheDocument();
    expect(within(dialog).getByText("Delete the webhook configurations listed below.")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Fix" }));
    expect(await screen.findByText("Nothing to do")).toBeInTheDocument();
    expect(screen.getByText("Nothing to do: every Service exists.").closest(".pf-v6-c-alert")).toHaveClass("pf-m-info");
    expect(api.bodies["POST /api/diagnostics/fix"]).toEqual([{ problemId: "delete-stale-webhooks" }]);
    expect(api.calls.filter((c) => c === "GET /api/diagnostics").length).toBe(2);
  });

  it("prerequisites refusals are warnings with the server's blockers", async () => {
    stubApi({
      "/api/diagnostics": result([fixable]),
      "POST /api/diagnostics/fix": () => jsonResponse({ success: false, errorCode: "prerequisites", message: "rhods-operator has no ready pod.", logs: [] }, 422),
    });
    renderPage(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    expand(fixable.title);
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Fix" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText("rhods-operator has no ready pod.")).toBeInTheDocument();
  });

  it("read-only users get a disabled Fix with the reason", async () => {
    stubApi({ "/api/diagnostics": result([fixable]), "/api/user/permissions": { canMutate: false, user: "v" } });
    renderPage(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    expand(fixable.title);
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Fix" })).toHaveAttribute("aria-disabled", "true");
  });

  it("a teammate's running operation disables Fix with the reason (R4b c, N1)", async () => {
    stubApi({ "/api/diagnostics": result([fixable]) });
    renderPage(<TroubleshootingPage />, "/", {
      operation: async () => ({ inProgress: true, operation: { id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } }),
    });
    await screen.findByText(fixable.title);
    expand(fixable.title);
    const fix = screen.getByRole("button", { name: "Fix" });
    await waitFor(() => expect(fix).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(fix);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/alice is running "Update to nightly"/);
  });

  it("an expired session disables Fix at once (R4b c)", async () => {
    stubApi({
      "/api/diagnostics": result([fixable]),
      "/api/user/permissions": () => new Response("<html>Log In</html>", { status: 403, headers: { "Content-Type": "text/html" } }),
    });
    renderPage(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    expand(fixable.title);
    const fix = screen.getByRole("button", { name: "Fix" });
    fireEvent.mouseEnter(fix);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/session has expired/);
  });

  it("a cluster_busy refusal asks the server which operation holds the lock", async () => {
    stubApi({
      "/api/diagnostics": result([fixable]),
      "POST /api/diagnostics/fix": () => jsonResponse({ error: "alice is running \"Update to nightly\". Wait for it to finish.", errorCode: "cluster_busy" }, 409),
    });
    let answer: OperationStatusResponse = { inProgress: false, operation: null };
    const fetchOperation = vi.fn(async () => answer);
    renderPage(<TroubleshootingPage />, "/", { operation: fetchOperation });
    await screen.findByText(fixable.title);
    expand(fixable.title);
    await waitFor(() => expect(screen.getByRole("button", { name: "Fix" })).not.toHaveAttribute("aria-disabled"));
    const before = fetchOperation.mock.calls.length;
    answer = { inProgress: true, operation: { id: "op1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } };
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Fix" }));
    expect(await screen.findByText("Cluster busy")).toBeInTheDocument();
    await waitFor(() => expect(fetchOperation.mock.calls.length).toBeGreaterThan(before));
    await waitFor(() => expect(screen.getByRole("button", { name: "Fix" })).toHaveAttribute("aria-disabled", "true"));
  });

  it("the summary counts the listed problems by severity (live: '4 issues' vs 'Problems (6)')", async () => {
    const info: Problem = { ...guidance, id: "webhook-service-missing", severity: "info", title: "A webhook Service is missing" };
    const info2: Problem = { ...guidance, id: "mlflow-unmanaged", severity: "info", title: "mlflow-operator is unmanaged" };
    stubApi({ "/api/diagnostics": result([guidance, pods, fixable, { ...pods, id: "p2", title: "trustyai restarting" }, info, info2]) });
    renderPage(<TroubleshootingPage />);
    expect(await screen.findByText("4 problems need attention · 2 informational")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Need attention (4)" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Informational (2)" })).toBeInTheDocument();
    expect(screen.queryByText(/Problems \(/)).not.toBeInTheDocument();
  });

  it.each([
    [[], "Some health checks did not pass"],
    [[{ severity: "info" }], "No problems need attention · 1 informational"],
    [[{ severity: "warning" }], "1 problem needs attention"],
  ])("problemSummary(%j)", (problems, title) => {
    expect(problemSummary(problems as Problem[]).title).toBe(title);
  });

  it("a failed scan is classified and offers Retry", async () => {
    stubApi({ "/api/diagnostics": () => jsonResponse({ error: "context deadline exceeded", errorCode: "timeout" }, 504) });
    renderPage(<TroubleshootingPage />);
    expect(await screen.findByText(/^The request timed out/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("the copied report marks manual and automatic fixes and lists objects", () => {
    const text = buildDiagnosticReport(result([guidance, fixable]));
    expect(text).toContain("[INFO] Platform modules");
    expect(text).toContain("Objects: CustomResourceDefinition mcpservers.mcp.x-k8s.io");
    expect(text).toContain("(manual)");
    expect(text).toContain("(automatic fix available)");
  });
});
