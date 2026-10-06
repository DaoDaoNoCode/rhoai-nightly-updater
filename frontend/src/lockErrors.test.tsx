import React from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describeError } from "./errors";
import { outcomeTitle, outcomeVariant } from "./outcomes";
import { ApiError, streamUpdate } from "./services/api";
import { useOperation } from "./state/AppState";
import { renderWithApp } from "./test/providers";
import { jsonResponse } from "./test/utils";

const MESSAGE = "Cannot verify that no other updater pod is running an operation (the operation lock could not be read or written). Nothing was changed; try again.";

describe("lock_unavailable (the backend fails closed on the cross-pod lock)", () => {
  it("is a retryable warning, never a success", () => {
    const d = describeError(new ApiError({ status: 503, errorCode: "lock_unavailable", message: MESSAGE }));
    expect(d).toMatchObject({ title: "Could not check for an operation in another updater pod", variant: "warning" });
    expect(d.hint).toMatch(/Nothing was changed. Retry/);
    expect(outcomeVariant({ success: false, errorCode: "lock_unavailable" })).toBe("warning");
    expect(outcomeTitle({ success: false, errorCode: "lock_unavailable", message: MESSAGE })).toMatch(/try again/);
    expect(outcomeTitle({ success: false, errorCode: "lock_lost", message: "x" })).toMatch(/another updater pod took over/);
  });

  it("ends a started run as a rejected failure (nothing changed), not as running or succeeded", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: MESSAGE, errorCode: "lock_unavailable" }, 503)));
    const Probe: React.FC = () => {
      const op = useOperation();
      const outcome = op.run?.outcome;
      return (
        <>
          <button onClick={() => op.start("update", (h) => streamUpdate("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", h.onStep, h.onDone, h.onDetach))}>start</button>
          <pre data-testid="probe">{JSON.stringify({ running: op.running, status: outcome?.status, rejected: outcome?.status === "failed" ? outcome.rejected : undefined, code: outcome?.status === "failed" ? outcome.errorCode : undefined })}</pre>
        </>
      );
    };
    renderWithApp(<Probe />);
    fireEvent.click(screen.getByText("start"));
    await waitFor(() => expect(JSON.parse(screen.getByTestId("probe").textContent || "{}")).toEqual({ running: false, status: "failed", rejected: true, code: "lock_unavailable" }));
  });
});
