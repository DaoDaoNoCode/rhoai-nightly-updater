import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { ResetModal } from "./ResetModal";
import { durationText } from "./planText";
import { stubApi } from "../../test/apiStub";
import { jsonResponse } from "../../test/utils";
import type { DSCPlan } from "../../types";

const isOff = (el: HTMLElement) => el.hasAttribute("disabled") || el.getAttribute("aria-disabled") === "true";

const base = { name: "default-dsc", uid: "u-1", token: "0123456789abcdef01234567", keepEnabled: true };
const soft: DSCPlan = { ...base, action: "soft-reset", changes: [{ path: "ray", from: "Removed", fromSet: true, to: "Managed", toSet: true, toggle: true, effect: "deploys" }],
  protectedFields: ["aiHub.instancesNamespace", "workbenches.workbenchNamespace"] };

/** The live case (RHOAI 3.6.0): MLflow in use and two modules the tool cannot check, with what stops working meanwhile. */
const blockedHard: DSCPlan = {
  ...base, action: "hard-reset", tornDown: ["aigateway", "aipipelines", "kserve", "mlflowoperator"], changes: [], newObject: "kind: DataScienceCluster\n", deleteTimeoutSeconds: 600,
  blocked: [
    { component: "aigateway", reasons: ["can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there"] },
    { component: "mcplifecycleoperator", reasons: ["can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there"] },
    { component: "mlflowoperator", reasons: ["in use by MLflow mlflow, so its removal would hang; delete MLflow first (Environment page)"] },
  ],
  stopsWorking: [{ resource: "pipeline server juntao-test/dspa", components: ["aipipelines"] }],
};

const hard: DSCPlan = {
  ...base, action: "hard-reset", tornDown: ["aipipelines", "kserve", "ray"], stayRemoved: ["kueue"], kept: ["aigateway"], newObject: "kind: DataScienceCluster\n", deleteTimeoutSeconds: 90,
  changes: [{ path: "kueue", from: "Unmanaged", fromSet: true, to: "Removed", toSet: true, toggle: true, effect: "removes" }],
  stopsWorking: [
    { resource: "pipeline server team/dspa", components: ["aipipelines"] },
    { resource: "playground server team/lsd", components: ["dashboard", "ogx"] },
    { resource: "LLMInferenceService team/qwen", components: ["kserve"] },
  ],
  conversionCRDs: [{ crd: "guardrails.trustyai.opendatahub.io", component: "trustyai" }, { crd: "nemoguardrails.trustyai.opendatahub.io", component: "trustyai" }],
};

function open(plans: { soft?: DSCPlan; hard?: DSCPlan }) {
  stubApi({ "GET /api/components/dsc/plan": (url: string) => jsonResponse(url.includes("hard-reset") ? plans.hard : plans.soft ?? soft) });
  render(<ResetModal name="default-dsc" disabledReason={null} onClose={() => {}} onDone={() => {}} />);
  return screen.getByRole("dialog", { name: "Reset to the release default" });
}

describe("ResetModal", () => {
  it("blocked hard reset: why comes right after the choices, and what it would do is collapsed", async () => {
    const dialog = open({ hard: blockedHard });
    fireEvent.click(within(dialog).getByLabelText("Hard reset"));
    const alert = (await within(dialog).findByText("This change is not possible now")).closest(".pf-v6-c-alert")!;
    const reasons = within(alert as HTMLElement).getByRole("list", { name: "Why the change is not possible" });
    expect(within(reasons).getAllByRole("listitem")).toHaveLength(3);
    expect(within(reasons).getByText(/so its removal would hang; delete MLflow first/)).toBeInTheDocument();
    const details = within(dialog).getByRole("button", { name: "What a hard reset would do" });
    expect(details).toHaveAttribute("aria-expanded", "false");
    // The alert comes before the details.
    expect(alert.compareDocumentPosition(details) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
    expect(isOff(within(dialog).getByRole("button", { name: "Hard reset" }))).toBe(true);
    fireEvent.click(details);
    expect(within(dialog).getByText(/tears down every enabled component/)).toBeVisible();
  });

  it("hard reset: details open, the plan's timeout, grouped sections with counts and nothing fixed", async () => {
    const dialog = open({ hard });
    fireEvent.click(within(dialog).getByLabelText("Hard reset"));
    expect(await within(dialog).findByRole("button", { name: "What a hard reset would do" })).toHaveAttribute("aria-expanded", "true");
    expect(within(dialog).getByText(/tears down every enabled component \(3\)/)).toBeInTheDocument();
    expect(within(dialog).getByText(/not gone within 1 minute 30 seconds, nothing is created/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/10 minutes/)).not.toBeInTheDocument();
    // No fixed list of resource kinds: only what the plan names.
    expect(within(dialog).queryByText(/notebooks, pipeline servers, models/)).not.toBeInTheDocument();
    expect(within(dialog).getByText(/^Stay Removed/)).toBeInTheDocument();
    expect(within(dialog).getByText(/They come back as they are now/)).toBeInTheDocument();
    for (const name of ["Stops working until its component is back (3)", "CRDs that cannot be read at another version meanwhile (2)",
      "Changes in the new DataScienceCluster (1)", "The DataScienceCluster it creates"]) {
      expect(within(dialog).getByRole("button", { name })).toHaveAttribute("aria-expanded", "false");
    }
    fireEvent.click(within(dialog).getByRole("button", { name: "Stops working until its component is back (3)" }));
    const stops = within(dialog).getByRole("list", { name: "What stops working until its component is back" });
    expect(within(stops).getByText(/playground server team\/lsd: needs/)).toHaveTextContent("playground server team/lsd: needs dashboard, ogx");
    fireEvent.click(within(dialog).getByRole("button", { name: "CRDs that cannot be read at another version meanwhile (2)" }));
    const crds = within(dialog).getByRole("list", { name: "CRDs that cannot be read at another version meanwhile" });
    expect(within(crds).getAllByRole("listitem")[0]).toHaveTextContent("guardrails.trustyai.opendatahub.io: served by trustyai");
    // Not blocked: confirm by typing the name.
    expect(within(dialog).getByRole("textbox")).toBeInTheDocument();
  });

  it("hard reset without changes says so instead of an empty list", async () => {
    const dialog = open({ hard: { ...hard, changes: [], stopsWorking: [], conversionCRDs: [] } });
    fireEvent.click(within(dialog).getByLabelText("Hard reset"));
    expect(await within(dialog).findByText("Changes in the new DataScienceCluster: none; it has the same settings as now.")).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: /Stops working/ })).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: /CRDs that cannot be read/ })).not.toBeInTheDocument();
  });

  it("names the fields a reset keeps from the plan, not a fixed list", async () => {
    const dialog = open({});
    expect(await within(dialog).findByRole("button", { name: "What a soft reset would do" })).toHaveAttribute("aria-expanded", "true");
    const choices = within(dialog).getByRole("radiogroup");
    expect(choices).toHaveTextContent("Both keep the value of aiHub.instancesNamespace, workbenches.workbenchNamespace.");
    expect(dialog).not.toHaveTextContent(/Namespaces of workbenches and the AI hub/);
  });

  it("says nothing about kept fields when the DataScienceCluster sets none", async () => {
    const dialog = open({ soft: { ...soft, protectedFields: [] } });
    await within(dialog).findByRole("list", { name: "Changes of the reset" });
    expect(dialog).not.toHaveTextContent(/keep the value of/);
  });
});

describe("durationText", () => {
  it.each([[600, "10 minutes"], [60, "1 minute"], [90, "1 minute 30 seconds"], [45, "45 seconds"], [1, "1 second"]])("%i seconds is %s", (s, want) => {
    expect(durationText(s)).toBe(want);
  });
});
