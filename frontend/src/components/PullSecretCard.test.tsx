import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { PullSecretSetup } from "./PullSecretCard";
import { ClusterSetupCard } from "./StatusCards";
import { SetupModal } from "./PrerequisitesPanel";
import { prerequisitesMet } from "../utils";
import { nightlyStatus, renderWithApp, IDLE_OPERATION, settleApps } from "../test/providers";
import { useOperation } from "../state/AppState";
import { streamUpdate } from "../services/api";
import { sseResponse } from "../test/utils";

const missing = { exists: false, valid: false, hasQuayAuth: false } as unknown as React.ComponentProps<typeof PullSecretSetup>["pullSecret"];
const token = btoa("user:password");

const StartButton: React.FC = () => {
  const op = useOperation();
  return (
    <button onClick={() => op.start("update", (h) => streamUpdate("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", h.onStep, h.onDone, h.onDetach))}>
      start
    </button>
  );
};

afterEach(() => vi.unstubAllGlobals());

async function saveButton(): Promise<HTMLElement> {
  fireEvent.change(await screen.findByLabelText(/Auth token for quay.io\/rhoai/), { target: { value: token } });
  return screen.getByRole("button", { name: "Create secret" });
}

describe("PullSecretSetup gating", () => {
  it("is enabled when nothing blocks it", async () => {
    renderWithApp(<PullSecretSetup pullSecret={missing} />);
    await settleApps();
    const save = await saveButton();
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled", "true"));
    await settleApps();
  });

  it("is disabled while this tab's own update stream runs, although GET /api/operation is not polled then", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    renderWithApp(<><StartButton /><PullSecretSetup pullSecret={missing} /></>, { operation: async () => IDLE_OPERATION });
    await settleApps();
    const save = await saveButton();
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("button", { name: "start" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Create secret" })).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(screen.getByRole("button", { name: "Create secret" }));
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/still running/);
    // End the stream inside act, so the run's end and the reads it starts
    // (status, operation, Dashboard Dev state) are applied in act, then
    // wait for those reads before looking.
    await act(async () => { sse.close(); });
    await settleApps();
    expect(screen.getByRole("button", { name: "Create secret" })).not.toHaveAttribute("aria-disabled", "true");
  });

  it("is disabled for a read-only user", async () => {
    renderWithApp(<PullSecretSetup pullSecret={missing} />, { permissions: async () => ({ canMutate: false, user: "viewer" }) });
    await settleApps();
    await saveButton();
    await waitFor(() => expect(screen.getByRole("button", { name: "Create secret" })).toHaveAttribute("aria-disabled", "true"));
    await settleApps();
  });
});

describe("PullSecretSetup actions of an existing secret", () => {
  const ready = { exists: true, valid: true, hasQuayAuth: true } as unknown as React.ComponentProps<typeof PullSecretSetup>["pullSecret"];

  it("offers Test with Quay and Replace token as buttons of the same weight; Replace token is the danger one", async () => {
    renderWithApp(<PullSecretSetup pullSecret={ready} />);
    await settleApps();
    const test = await screen.findByRole("button", { name: "Test with Quay" });
    const replace = screen.getByRole("button", { name: "Replace token" });
    for (const b of [test, replace]) {
      expect(b).toHaveClass("pf-m-secondary");
      expect(b).not.toHaveClass("pf-m-link");
    }
    expect(replace).toHaveClass("pf-m-danger");
    expect(test).not.toHaveClass("pf-m-danger");
    // Both in one ActionListGroup: PatternFly's action-to-action spacing.
    expect(test.closest(".pf-v6-c-action-list__group")).toBe(replace.closest(".pf-v6-c-action-list__group"));
    expect(test.closest(".pf-v6-c-action-list__group")).not.toBeNull();
    await settleApps();
  });

  it("opens the form as the confirmation step: Replace token (danger) and Cancel, with focus moving in and back", async () => {
    renderWithApp(<PullSecretSetup pullSecret={ready} />);
    await settleApps();
    fireEvent.click(await screen.findByRole("button", { name: "Replace token" }));

    // The toggle is hidden while the form is open; Test with Quay stays.
    const input = screen.getByLabelText(/Auth token for quay.io\/rhoai/);
    expect(input).toHaveFocus();
    expect(screen.getByRole("button", { name: "Test with Quay" })).toBeInTheDocument();
    const submit = screen.getByRole("button", { name: "Replace token" });
    expect(submit).toHaveClass("pf-m-danger");
    expect(submit).not.toHaveClass("pf-m-secondary");
    expect(submit).toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByText(/Replace secret|Cancel replacing/)).not.toBeInTheDocument();

    fireEvent.change(input, { target: { value: token } });
    await waitFor(() => expect(screen.getByRole("button", { name: "Replace token" })).not.toHaveAttribute("aria-disabled", "true"));

    const cancel = screen.getByRole("button", { name: "Cancel" });
    expect(cancel).toHaveClass("pf-m-link");
    fireEvent.click(cancel);
    expect(screen.queryByLabelText(/Auth token for quay.io\/rhoai/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Replace token" })).toHaveFocus();
    await settleApps();
  });
});

// The gitops profile pulls with the cluster's own pull secrets: a missing
// additional-pull-secret is not used there, so it is no error and no
// unfinished setup. The full profile is unchanged.
describe("A missing pull secret in the gitops profile", () => {
  const noSecret = nightlyStatus({ pullSecret: { exists: false, valid: false, detail: "Secret not found" } });

  it("is not used, not missing", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={noSecret.pullSecret} readOnly />).settled();
    expect(screen.getByText("Not used")).toBeInTheDocument();
    expect(screen.queryByText("Missing")).not.toBeInTheDocument();
    expect(screen.getByText(/the cluster's pull secret is used/)).toBeInTheDocument();
    cleanup();
    // The backend does not read the secret there, so whatever the status
    // says about it is not shown as a problem.
    await renderWithApp(<PullSecretSetup pullSecret={{ exists: true, valid: false, detail: "Credentials rejected" }} readOnly />).settled();
    expect(screen.getByText("Not used")).toBeInTheDocument();
    expect(screen.queryByText(/can't pull nightly images/)).not.toBeInTheDocument();
    cleanup();
    await renderWithApp(<PullSecretSetup pullSecret={noSecret.pullSecret} />).settled();
    expect(screen.getByText("Missing")).toBeInTheDocument();
  });

  it("does not hold back the cluster setup or Update", async () => {
    expect(prerequisitesMet(noSecret, true)).toBe(true);
    expect(prerequisitesMet(noSecret)).toBe(false);
    // The gitops profile does not read it at all; the full profile still checks it.
    expect(prerequisitesMet(nightlyStatus({ pullSecret: { exists: true, valid: false } }), true)).toBe(true);
    expect(prerequisitesMet(nightlyStatus({ pullSecret: { exists: true, valid: false } }))).toBe(false);
    await renderWithApp(<ClusterSetupCard status={noSecret} onStatusRefresh={vi.fn()} onShowInstructions={vi.fn()} collapsible readOnly />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
  });

  it("says so in the setup instructions", async () => {
    await renderWithApp(<SetupModal isOpen onClose={vi.fn()} status={noSecret} gitOpsProfile />).settled();
    expect(screen.getByText(/Not used: the gitops profile uses the cluster's pull secret/)).toBeInTheDocument();
    expect(screen.queryByText(/additional-pull-secret is missing/)).not.toBeInTheDocument();
  });
});

describe("The internal registry's copy of the token (ROSA HCP)", () => {
  afterEach(cleanup);
  const valid = { exists: true, valid: true };
  const copy = { name: "rhoai-nightly-updater-pull", namespace: "redhat-ods-applications", exists: true, ours: true, current: true };

  it.each([
    [copy, "Workbench images: the internal registry can pull nightly images", "It uses Secret rhoai-nightly-updater-pull in redhat-ods-applications, a copy of this token."],
    [{ ...copy, current: false }, "Workbench images: the internal registry has an older token", "Save the token again with Replace token, or run Update or Refresh"],
    [{ ...copy, exists: false, ours: false, current: false }, "Workbench images: the internal registry cannot pull nightly images", "copies it to Secret rhoai-nightly-updater-pull in redhat-ods-applications."],
    [{ ...copy, ours: false, current: false }, "Workbench images: Secret rhoai-nightly-updater-pull in redhat-ods-applications was not created by this updater", "The updater leaves it as it is"],
    [{ name: "rhoai-nightly-updater-pull", noApplicationsNamespace: true, exists: false, ours: false, current: false }, "Workbench images: RHOAI is not installed yet", "Once it is, the updater copies the token"],
    [{ name: "rhoai-nightly-updater-pull", exists: false, ours: false, current: false, problem: "Could not read Secret x: forbidden" }, "Workbench images: could not check whether the internal registry can pull nightly images", "Could not read Secret x: forbidden."],
  ])("says plainly whether workbench images can be pulled (%#)", async (registry, title, body) => {
    renderWithApp(<PullSecretSetup pullSecret={{ ...valid, registry }} controlPlane="hosted" />);
    await settleApps();
    expect(screen.getByText(title)).toBeInTheDocument();
    expect(screen.getByText(title).closest(".pf-v6-c-alert")).toHaveTextContent(body);
  });

  it("is not shown without the copy's state (self-managed, gitops profile)", async () => {
    renderWithApp(<PullSecretSetup pullSecret={valid} />);
    await settleApps();
    expect(screen.queryByText(/Workbench images/)).not.toBeInTheDocument();
    cleanup();
    renderWithApp(<PullSecretSetup pullSecret={{ ...valid, registry: copy }} readOnly />);
    await settleApps();
    expect(screen.queryByText(/Workbench images/)).not.toBeInTheDocument();
  });
});
