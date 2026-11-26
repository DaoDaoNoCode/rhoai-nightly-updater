import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ActivityLog, activityCategory, activityReason } from "./ActivityLog";
import type { ActivityEntry } from "../types";

const refused: ActivityEntry = {
  timestamp: "2026-10-06T03:52:08Z",
  user: "alice",
  action: "update",
  detail: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:0aa1",
  success: false,
  reason: "A Dashboard Dev session is active (dashboard-operator is paused). Update was not started and nothing was changed.",
  label: "Update to nightly failed",
  category: "operator",
  build: "rhoai-3.6 · 0aa1",
};

describe("ActivityLog", () => {
  it("shows why a failed operation failed, without a second (failed) suffix", () => {
    render(<ActivityLog activity={[refused]} />);
    expect(screen.getByText("Update to nightly failed")).toBeTruthy();
    expect(screen.queryByText("(failed)")).toBeNull();
    expect(screen.getByText(/A Dashboard Dev session is active/)).toBeTruthy();
  });

  it("keeps the (failed) suffix for older entries whose label is the success wording", () => {
    render(<ActivityLog activity={[{ ...refused, label: "Updated to nightly", reason: undefined }]} />);
    expect(screen.getByText("(failed)")).toBeTruthy();
  });

  it("says so when nothing is recorded yet, without a filter", () => {
    render(<ActivityLog activity={[]} />);
    expect(screen.getByText("No activity yet")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Activity type/ })).toBeNull();
  });

  it("files quick resources under Environment", async () => {
    expect(activityCategory({ ...refused, action: "teardown-minio", category: undefined })).toBe("test-resources");
    expect(activityCategory({ ...refused, action: "deploy-dashboard-main", category: undefined })).toBe("dashboard-dev");
    render(
      <ActivityLog
        activity={[{ ...refused, action: "setup-minio", success: true, label: "S3 storage set up", category: "test-resources", reason: undefined }]}
        defaultCategory="all"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Activity type: All" }));
    fireEvent.click(screen.getByRole("option", { name: /Environment/ }));
    expect(screen.getByText("S3 storage set up")).toBeTruthy();
    // The menu closes on PatternFly Popper's hide timer (setTimeout).
    await waitFor(() => expect(screen.queryByRole("listbox")).not.toBeInTheDocument());
  });
});

describe("the reason of a cancelled project action", () => {
  const entry: ActivityEntry = { timestamp: "2026-11-25T10:00:00Z", user: "alice", action: "setup-workbench", detail: "workbench in team-a", success: false, reason: "Cancelled by the user who started it", cancelledBy: "starter" };

  it("says you to the user who started it, and the stored words to anyone else", () => {
    expect(activityReason(entry, "alice")).toBe("Cancelled by you");
    expect(activityReason(entry, "root")).toBe("Cancelled by the user who started it");
    expect(activityReason(entry, null)).toBe("Cancelled by the user who started it");
  });

  it("never turns a cluster-admin's cancel into you", () => {
    const byAdmin: ActivityEntry = { ...entry, reason: "Cancelled by a cluster-admin", cancelledBy: "admin" };
    expect(activityReason(byAdmin, "alice")).toBe("Cancelled by a cluster-admin");
  });
});
