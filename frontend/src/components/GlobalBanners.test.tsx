import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { USER_PERMISSIONS, renderWithApp } from "../test/providers";
import { ApiError } from "../services/api";
import { formatClockTime } from "../utils";
import type { ServerOperation } from "../types";

const remoteUpdate: ServerOperation = {
  id: "op1", type: "update", label: "Update to nightly", user: "qa-user",
  target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64),
  startedAt: "2026-10-06T14:27:00Z", step: "verify_installplan", stepStatus: "running",
  message: "CSV rhods-operator.3.6.0: Installing (InstallWaiting)", pod: "updater-old", remote: true,
};

describe("the banner of an operation another updater pod runs", () => {
  it("ends the latest progress with a full stop, also after a parenthesis", async () => {
    renderWithApp(<GlobalBanners />, { operation: async () => ({ inProgress: true, operation: remoteUpdate }) }, "/components");
    expect(await screen.findByText(/on updater pod updater-old/)).toBeInTheDocument();
    expect(screen.getByText(/Latest progress: CSV rhods-operator\.3\.6\.0: Installing \(InstallWaiting\)\. Another updater pod runs it/)).toBeInTheDocument();
  });
});

describe("the permissions notice", () => {
  const scope = "Your sign-in cannot make changes: it was made before this updater acted with your own OpenShift permissions. Log out and log in again.";
  const expiring = "Your sign-in expires in 3 minutes, and a change may take up to 21 minutes. Log out and log in again, then retry.";

  it.each([
    ["a sign-in from before 4.0.0", scope],
    ["a sign-in about to expire", expiring],
  ])("shows the backend's reason with Log out for %s, not read-only access", async (_name, reason) => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "admin", role: "admin", reason, reasonCode: "sign_in_again" }) }, "/components");
    expect(await screen.findByText("Sign in again to make changes", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(`${reason} You can still view everything.`)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Log out" })).toHaveAttribute("href", "/oauth/sign_out");
    expect(screen.queryByText(/Read-only access/)).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("shows another backend reason without Log out", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "admin", role: "admin", reason: "Changes are paused for maintenance" }) }, "/components");
    expect(await screen.findByText("Changes are paused for maintenance. You can still view everything.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("keeps the read-only notice for a cluster-admin whose answer gives no reason", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "viewer", role: "admin" }) }, "/components");
    expect(await screen.findByText("Read-only access", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/requires the cluster-admin role/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Sign in again/)).not.toBeInTheDocument();
  });

  // Per-user views: read-only is what a user is, and their pages already show their view.
  it.each([
    ["role user", { canMutate: false, user: "dev-user", role: "user" as const }],
    ["no role (fail closed)", { canMutate: false, user: "dev-user" }],
  ])("shows a user who is not a cluster-admin no read-only notice (%s)", async (_name, answer) => {
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, { permissions: async () => answer }, "/test-resources");
    await screen.findByText("loaded");
    await act(async () => {});
    expect(screen.queryByText(/Read-only access/)).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("still warns a user when their permissions could not be checked", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => { throw new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot verify" }); } }, "/");
    expect(await screen.findByText("Your permissions could not be checked; changes are disabled", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});

describe("the operation banner for a user who is not a cluster-admin", () => {
  const startedAt = "2026-10-06T14:05:00Z";
  const clock = formatClockTime(startedAt);

  it("says when an update started and what it is, never who runs it", async () => {
    renderWithApp(<GlobalBanners />, {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => ({ running: true, label: "Update to nightly", startedAt }),
    }, "/");
    expect(await screen.findByText(`An update is running since ${clock}`, { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/Update to nightly\. What this page shows may change until it finishes\./)).toBeInTheDocument();
    expect(screen.queryByText(/qa-user|Someone|on updater pod|View progress/)).not.toBeInTheDocument();
    expect(clock).toMatch(/^\d\d:\d\d$/);
  });

  it("shows nothing while no operation runs", async () => {
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => ({ running: false }),
    }, "/");
    await screen.findByText("loaded");
    await act(async () => {});
    expect(screen.queryByText(/is running/)).not.toBeInTheDocument();
  });
});

describe("notices only a cluster-admin can act on", () => {
  const outdated = async () => ({ version: "v5.0.0", commit: "c", buildDate: "d", templateOutdated: true, templateRevision: "7", expectedTemplateRevision: "8" });
  const newer = async () => ({ updateAvailable: true, majorUpgrade: false, current: "v5.0.0", latest: "v5.1.0" });

  it("shows the deployment and new-release notices to a cluster-admin", async () => {
    renderWithApp(<GlobalBanners />, { version: outdated, update: newer }, "/");
    expect(await screen.findByText("For admins: this updater's deployment is out of date", { exact: false })).toBeInTheDocument();
    expect(await screen.findByText("v5.1.0 is available", { exact: false })).toBeInTheDocument();
  });

  it("does not show them to a user who is not a cluster-admin", async () => {
    const update = vi.fn(newer);
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, { permissions: async () => USER_PERMISSIONS, version: outdated, update }, "/");
    await screen.findByText("loaded");
    // The release check itself is allowed for everyone; only the notice is a cluster-admin's.
    await waitFor(() => expect(update).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByText(/deployment is out of date/)).not.toBeInTheDocument();
    expect(screen.queryByText(/is available/)).not.toBeInTheDocument();
  });
});
