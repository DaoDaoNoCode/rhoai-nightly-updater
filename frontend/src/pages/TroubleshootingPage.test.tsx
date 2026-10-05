import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { TroubleshootingPage, buildDiagnosticReport } from "./TroubleshootingPage";
import { stubApi, jsonResponse } from "../test/apiStub";
import { resetPermissionsCache } from "../hooks/usePermissions";
import type { DiagnosticResult, Problem } from "../types";

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

beforeEach(() => resetPermissionsCache());

describe("Diagnostics (B2 contract)", () => {
  it("guidance-only problems show the objects and a copyable command, and no Fix button", async () => {
    stubApi({ "/api/diagnostics": result([guidance, pods]) });
    render(<TroubleshootingPage />);
    const card = (await screen.findByText(guidance.title)).closest(".pf-v6-c-card") as HTMLElement;
    expect(within(card).getByText("Manual fix")).toBeInTheDocument();
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
    render(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
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
    render(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Fix" }));
    expect(await screen.findByText("Blocked: prerequisites not met")).toBeInTheDocument();
    expect(screen.getByText("rhods-operator has no ready pod.")).toBeInTheDocument();
  });

  it("read-only users get a disabled Fix with the reason", async () => {
    stubApi({ "/api/diagnostics": result([fixable]), "/api/user/permissions": { canMutate: false, user: "v" } });
    render(<TroubleshootingPage />);
    await screen.findByText(fixable.title);
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: "Fix" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Fix" })).toHaveAttribute("aria-disabled", "true");
  });

  it("a failed scan is classified and offers Retry", async () => {
    stubApi({ "/api/diagnostics": () => jsonResponse({ error: "context deadline exceeded", errorCode: "timeout" }, 504) });
    render(<TroubleshootingPage />);
    expect(await screen.findByText("The request timed out")).toBeInTheDocument();
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
