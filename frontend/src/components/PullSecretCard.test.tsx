import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { PullSecretSetup } from "./PullSecretCard";
import { renderWithApp, IDLE_OPERATION } from "../test/providers";
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
