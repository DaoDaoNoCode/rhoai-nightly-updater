import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { PullSecretSetup } from "./PullSecretCard";
import { ClusterSetupCard } from "./StatusCards";
import { SetupModal } from "./PrerequisitesPanel";
import { prerequisitesMet } from "../utils";
import { nightlyStatus, renderWithApp, IDLE_OPERATION } from "../test/providers";
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
  fireEvent.change(await screen.findByLabelText("quay.io/rhoai auth token"), { target: { value: token } });
  return screen.getByRole("button", { name: "Create secret" });
}

describe("PullSecretSetup gating (R7 L6)", () => {
  it("is enabled when nothing blocks it", async () => {
    renderWithApp(<PullSecretSetup pullSecret={missing} />);
    const save = await saveButton();
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled", "true"));
  });

  it("is disabled while this tab's own update stream runs, although GET /api/operation is not polled then", async () => {
    const sse = sseResponse();
    vi.stubGlobal("fetch", vi.fn(async () => sse.response));
    renderWithApp(<><StartButton /><PullSecretSetup pullSecret={missing} /></>, { operation: async () => IDLE_OPERATION });
    const save = await saveButton();
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("button", { name: "start" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Create secret" })).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(screen.getByRole("button", { name: "Create secret" }));
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/still running/);
    sse.close();
  });

  it("is disabled for a read-only user", async () => {
    renderWithApp(<PullSecretSetup pullSecret={missing} />, { permissions: async () => ({ canMutate: false, user: "viewer" }) });
    await saveButton();
    await waitFor(() => expect(screen.getByRole("button", { name: "Create secret" })).toHaveAttribute("aria-disabled", "true"));
  });
});

// The gitops profile pulls with the cluster's own pull secrets: a missing
// additional-pull-secret is not used there, so it is no error and no
// unfinished setup. The full profile is unchanged.
describe("A missing pull secret in the gitops profile", () => {
  const noSecret = nightlyStatus({ pullSecret: { exists: false, valid: false, detail: "Secret not found" } });

  it("is not used, not missing", () => {
    renderWithApp(<PullSecretSetup pullSecret={noSecret.pullSecret} readOnly />);
    expect(screen.getByText("Not used")).toBeInTheDocument();
    expect(screen.queryByText("Missing")).not.toBeInTheDocument();
    expect(screen.getByText(/the cluster's pull secret is used/)).toBeInTheDocument();
    cleanup();
    // The backend does not read the secret there, so whatever the status
    // says about it is not shown as a problem.
    renderWithApp(<PullSecretSetup pullSecret={{ exists: true, valid: false, detail: "Credentials rejected" }} readOnly />);
    expect(screen.getByText("Not used")).toBeInTheDocument();
    expect(screen.queryByText(/can't pull nightly images/)).not.toBeInTheDocument();
    cleanup();
    renderWithApp(<PullSecretSetup pullSecret={noSecret.pullSecret} />);
    expect(screen.getByText("Missing")).toBeInTheDocument();
  });

  it("does not hold back the cluster setup or Update", () => {
    expect(prerequisitesMet(noSecret, true)).toBe(true);
    expect(prerequisitesMet(noSecret)).toBe(false);
    // The gitops profile does not read it at all; the full profile still checks it.
    expect(prerequisitesMet(nightlyStatus({ pullSecret: { exists: true, valid: false } }), true)).toBe(true);
    expect(prerequisitesMet(nightlyStatus({ pullSecret: { exists: true, valid: false } }))).toBe(false);
    renderWithApp(<ClusterSetupCard status={noSecret} onStatusRefresh={vi.fn()} onShowInstructions={vi.fn()} collapsible readOnly />);
    expect(screen.getByText("Ready")).toBeInTheDocument();
  });

  it("says so in the setup instructions", () => {
    renderWithApp(<SetupModal isOpen onClose={vi.fn()} status={noSecret} gitOpsProfile />);
    expect(screen.getByText(/Not used: the gitops profile uses the cluster's pull secret/)).toBeInTheDocument();
    expect(screen.queryByText(/additional-pull-secret is missing/)).not.toBeInTheDocument();
  });
});
