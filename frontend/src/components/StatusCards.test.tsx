import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { InstalledBuildCard } from "./StatusCards";
import { GlobalBanners } from "./GlobalBanners";
import type { LatestNightlyResponse, StatusResponse } from "../types";
import { nightlyStatus, renderWithApp } from "../test/providers";

function renderCard(status: StatusResponse, opts: { latest?: LatestNightlyResponse | null; blocker?: string | null; prerequisitesMet?: boolean } = {}) {
  const onUpdate = vi.fn();
  renderWithApp(
    <InstalledBuildCard
      status={status}
      latest={opts.latest ?? null}
      latestLoading={false}
      blocker={opts.blocker ?? null}
      prerequisitesMet={opts.prerequisitesMet ?? true}
      onUpdate={onUpdate}
      onChooseBuild={vi.fn()}
      onPreview={vi.fn()}
      onRedeploy={vi.fn()}
    />,
  );
  return { onUpdate };
}

const installedRow = () => screen.getByText("Installed").closest(".pf-v6-c-description-list__group") as HTMLElement;
const latestRow = () => screen.getByText(/^Latest/).closest(".pf-v6-c-description-list__group") as HTMLElement;

describe("InstalledBuildCard (A07-1)", () => {
  it("shows installed vs latest by tag, short digest, build date and dashboard commit, and offers Update", () => {
    const { onUpdate } = renderCard(nightlyStatus());
    expect(screen.getByText("Update available")).toBeInTheDocument();
    expect(within(installedRow()).getByText("rhoai-3.6")).toBeInTheDocument();
    expect(within(installedRow()).getByText("4eff06d60000")).toBeInTheDocument();
    expect(within(installedRow()).getByRole("link", { name: /dashboard commit 32a2131/ })).toHaveAttribute("href", "https://github.com/red-hat-data-services/odh-dashboard/commit/32a213123fbc434a602ba5b05e4a2022dd0d85d6");
    expect(within(latestRow()).getByText("0b3f9b4d1111")).toBeInTheDocument();
    expect(within(latestRow()).getByText(/built/)).toBeInTheDocument();
    // Wrap-safe build lines: items separated by space, never by a "·" that could start or end a line.
    expect(installedRow().textContent).not.toContain("·");
    expect(latestRow().textContent).not.toContain("·");
    fireEvent.click(screen.getByRole("button", { name: "Update to latest" }));
    expect(onUpdate).toHaveBeenCalledWith(expect.objectContaining({ digest: "sha256:" + "0b3f9b4d".padEnd(64, "1") }));
  });

  it("says Up to date and offers no primary Update when the digests match", () => {
    const s = nightlyStatus();
    s.nightly = { ...s.nightly!, latest: s.nightly!.installed, updateAvailable: false };
    renderCard(s);
    expect(screen.getByText("Up to date")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Update to latest/ })).not.toBeInTheDocument();
    expect(screen.getByText(/You have the newest rhoai-3.6 build/)).toBeInTheDocument();
  });

  it("explains a failed Quay check instead of guessing", () => {
    const s = nightlyStatus();
    s.nightly = { installed: s.nightly!.installed, error: "Could not read the latest rhoai-3.6 build from Quay: timeout" };
    renderCard(s);
    expect(screen.getByText("Not checked")).toBeInTheDocument();
    expect(screen.getByText(/Could not read the latest rhoai-3.6 build/)).toBeInTheDocument();
  });

  it("on a GA install offers to switch to the newest nightly", () => {
    const s = nightlyStatus({
      subscription: { name: "rhods-operator", source: "redhat-operators", channel: "stable-3.x", state: "AtLatestKnown" },
      csv: { name: "rhods-operator.3.5.1", version: "3.5.1", phase: "Succeeded" },
      nightly: undefined,
    });
    renderCard(s, { latest: { tag: "rhoai-3.6", image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "c".repeat(64) } });
    expect(screen.getByText("Stable release")).toBeInTheDocument();
    expect(screen.getByText("RHOAI 3.5.1")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Switch to latest nightly (rhoai-3.6)" })).toBeInTheDocument();
  });

  it("on a fresh cluster says nothing is installed and why Install is disabled", () => {
    const s = nightlyStatus({
      subscription: { name: "", source: "", channel: "", state: "Not Installed" },
      csv: { name: "", version: "", phase: "Not Found" },
      catalogSource: { exists: false, name: "", image: "", state: "" },
      pullSecret: { exists: false, valid: false },
      nightly: undefined,
    });
    renderCard(s, { prerequisitesMet: false });
    // One "Not installed" label (the verdict); the operator row says it in words (UX-Status-4).
    expect(screen.getAllByText("Not installed", { selector: ".pf-v6-c-label__text" }).length).toBe(1);
    expect(screen.getByText("rhods-operator: not installed")).toBeInTheDocument();
    expect(screen.getByText(/the RHOAI operator is not installed/)).toBeInTheDocument();
    const install = screen.getByRole("button", { name: /Install latest nightly/ });
    expect(install).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByText("Finish the cluster setup above first.")).toBeInTheDocument();
  });

  it("keeps Update available from a Failed CSV and recommends it (A07-5)", () => {
    renderCard(nightlyStatus({ csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Failed" } }));
    expect(screen.getByText("The operator is Failed")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update to latest" })).not.toHaveAttribute("aria-disabled", "true");
  });

  it("disables Update with the reason while another operation runs (A07-9)", () => {
    renderCard(nightlyStatus(), { blocker: 'alice is running "Update to nightly". Wait for it to finish.' });
    expect(screen.getByRole("button", { name: "Update to latest" })).toHaveAttribute("aria-disabled", "true");
  });
});

describe("GlobalBanners", () => {
  it("shows who is running what, and the step, on every page (A07-3)", async () => {
    renderWithApp(<GlobalBanners />, {
      operation: async () => ({ inProgress: true, operation: { id: "1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString(), step: "detect_channel", target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64) } }),
    }, "/components");
    expect(await screen.findByText(/alice is running "Update to nightly": step 5 of 8: Detect the channel/)).toBeInTheDocument();
    expect(screen.getByText("rhoai-3.6 · aaaaaaaaaaaa")).toBeInTheDocument();
  });

  it("explains an interrupted operation and lets the user dismiss it", async () => {
    renderWithApp(<GlobalBanners />, {
      operation: async () => ({ inProgress: false, operation: null, interrupted: { type: "update", label: "Update to nightly", user: "alice", startedAt: "2026-10-05T09:00:00Z", pod: "old-pod" } }),
    });
    expect(await screen.findByText('"Update to nightly" was interrupted')).toBeInTheDocument();
    // The explanation is one click away, so the title stays one line.
    expect(screen.queryByText(/run the same operation again/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Warning alert details/ }));
    expect(screen.getByText(/run the same operation again/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Close/ }));
    await waitFor(() => expect(screen.queryByText('"Update to nightly" was interrupted')).not.toBeInTheDocument());
  });

  it("shows a paused dashboard-operator with who, what and the RHOAI update warning (A04-1)", async () => {
    renderWithApp(<GlobalBanners />, {
      dashboard: async () => ({
        override: {
          active: true, operatorPaused: true, sessionRecorded: true, mode: "pr", prNumber: 222, flavor: "rhoai", startedBy: "bob",
          startedAt: "2026-10-04T10:00:00Z", stale: true, staleReasons: ["RHOAI version changed from 3.6.0 to 3.6.1"], dashboardDeleting: false,
        },
      }) as never,
    });
    expect(await screen.findByText("RHOAI was updated while the dashboard was paused")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Warning alert details/ }));
    expect(screen.getByText(/bob deployed PR #222 \(RHOAI build\)/)).toBeInTheDocument();
    expect(screen.getByText(/RHOAI version changed from 3.6.0 to 3.6.1\./)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open Dashboard Dev" })).toBeInTheDocument();
  });

  it("tells admins to run make upgrade when the template is outdated (A09-4)", async () => {
    renderWithApp(<GlobalBanners />, { version: async () => ({ version: "4503bb7d", commit: "x", buildDate: "d", templateRevision: "1", expectedTemplateRevision: "2", templateOutdated: true }) });
    expect(await screen.findByText(/this updater's deployment is out of date/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Info alert details/ }));
    expect(screen.getByText("make upgrade")).toBeInTheDocument();
  });

  it("explains unknown permissions with a Retry (503 is read-only, A02-3)", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => { throw Object.assign(new Error("x"), { name: "ApiError", status: 503, errorCode: "authorization_unavailable", message: "Cannot verify mutation permissions" }); } });
    expect(await screen.findByText("Your permissions could not be checked; changes are disabled")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("shows at most two notices and keeps the rest one click away", async () => {
    renderWithApp(<GlobalBanners />, {
      operation: async () => ({ inProgress: true, operation: { id: "1", type: "update", label: "Update to nightly", user: "alice", startedAt: new Date().toISOString() } }),
      permissions: async () => { throw Object.assign(new Error("x"), { name: "ApiError", status: 503, errorCode: "authorization_unavailable", message: "Cannot verify mutation permissions" }); },
      version: async () => ({ version: "4503bb7d", commit: "x", buildDate: "d", templateRevision: "1", expectedTemplateRevision: "2", templateOutdated: true }),
    }, "/components");
    expect(await screen.findByText(/alice is running "Update to nightly"/)).toBeInTheDocument();
    expect(await screen.findByText("Your permissions could not be checked; changes are disabled")).toBeInTheDocument();
    expect(screen.queryByText(/this updater's deployment is out of date/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "View 1 more notice" }));
    expect(screen.getByText(/this updater's deployment is out of date/)).toBeInTheDocument();
  });
});
