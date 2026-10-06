import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ActivityLog, activityCategory } from "./ActivityLog";
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

  it("files quick resources under Test resources", () => {
    expect(activityCategory({ ...refused, action: "teardown-minio", category: undefined })).toBe("test-resources");
    expect(activityCategory({ ...refused, action: "deploy-dashboard-main", category: undefined })).toBe("dashboard-dev");
    render(
      <ActivityLog
        activity={[{ ...refused, action: "setup-minio", success: true, label: "MinIO set up", category: "test-resources", reason: undefined }]}
        defaultCategory="all"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Test resources \(1\)/ }));
    expect(screen.getByText("MinIO set up")).toBeTruthy();
  });
});
