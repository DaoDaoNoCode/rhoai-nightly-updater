import React from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { OperationProgress } from "./OperationProgress";
import { useOperation } from "../state/AppState";
import { renderWithApp } from "../test/providers";
import type { CompletedOperation, OperationStatusResponse, ServerOperation } from "../types";

const bobUpdate: ServerOperation = {
  id: "op-bob", type: "update", label: "Update to nightly", user: "bob",
  target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "b".repeat(64),
  startedAt: "2026-10-05T10:00:00Z", step: "verify_installplan", stepStatus: "running", message: "Waiting for the CSV",
};

const Poll: React.FC = () => {
  const op = useOperation();
  return <button onClick={op.refreshServerOperation}>poll</button>;
};

/** A teammate's update is followed from the server, then the server reports it gone. */
async function watchBobFinish(lastCompleted?: CompletedOperation) {
  let answer: OperationStatusResponse = { inProgress: true, operation: bobUpdate };
  renderWithApp(<><OperationProgress /><Poll /></>, { operation: async () => answer });
  expect(await screen.findByText(/Started by bob/)).toBeInTheDocument();
  answer = { inProgress: false, operation: null, lastCompleted };
  fireEvent.click(screen.getByText("poll"));
}

describe("OperationProgress for an operation another user ran (N4)", () => {
  it("does not claim success when the server sends no result (older backend)", async () => {
    await watchBobFinish(undefined);
    expect(await screen.findByText("Update finished (outcome unknown, see the activity log)")).toBeInTheDocument();
    expect(screen.queryByText("Update complete")).not.toBeInTheDocument();
  });

  it("shows the failure and the server's message", async () => {
    await watchBobFinish({ id: "op-bob", type: "update", label: "Update to nightly", user: "bob", startedAt: bobUpdate.startedAt, success: false, message: "The new CSV failed; the previous operator was restored." });
    expect(await screen.findByText("The new CSV failed; the previous operator was restored.")).toBeInTheDocument();
    expect(screen.getByText("Update failed")).toBeInTheDocument();
    expect(screen.queryByText("Update complete")).not.toBeInTheDocument();
  });

  it("shows success when the server reports it", async () => {
    await watchBobFinish({ id: "op-bob", type: "update", label: "Update to nightly", user: "bob", startedAt: bobUpdate.startedAt, success: true, message: "Updated" });
    await waitFor(() => expect(screen.getByText("Update complete")).toBeInTheDocument());
  });
});
