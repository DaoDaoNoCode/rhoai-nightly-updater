import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { UpdatePanel } from "./UpdatePanel";
import { useClusterStatus } from "../state/AppState";
import { nightlyStatus, renderWithApp, settleApps } from "../test/providers";
import { BACKGROUND_POLL_MS } from "../constants";

const START = new Date("2026-10-06T14:30:00Z").getTime();
const tick = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

// As StatusPage passes it: the status from the app state, polled every minute.
const FromAppState: React.FC = () => {
  const { status } = useClusterStatus();
  return <UpdatePanel status={status} latest={null} onRequestUpdate={vi.fn()} />;
};

beforeEach(() => {
  vi.useFakeTimers({ now: START, toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
});
afterEach(() => {
  vi.useRealTimers();
});

describe("the warning that someone else changed the operator", () => {
  it("counts up with the status polls and goes away 10 minutes after their change, also when the status does not change", async () => {
    // alice updated 2 minutes before the page opened; every poll answers the same status.
    const status = nightlyStatus({
      activity: [{ timestamp: new Date(START - 2 * 60_000).toISOString(), user: "alice", action: "update", detail: "", success: true, category: "operator", label: "Update to nightly" }],
    });
    // A new object per answer, as the API's JSON is.
    renderWithApp(<FromAppState />, { status: async () => structuredClone(status) });
    await tick(0);
    await tick(0);
    expect(screen.getByText("alice changed the operator 2 min ago")).toBeInTheDocument();
    for (let i = 0; i < 3; i++) await tick(BACKGROUND_POLL_MS);
    expect(screen.getByText("alice changed the operator 5 min ago")).toBeInTheDocument();
    for (let i = 0; i < 6; i++) await tick(BACKGROUND_POLL_MS);
    expect(screen.queryByText(/alice changed the operator/)).not.toBeInTheDocument();
    await settleApps();
  });
});
