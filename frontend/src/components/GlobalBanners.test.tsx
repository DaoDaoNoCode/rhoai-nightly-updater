import React from "react";
import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { renderWithApp } from "../test/providers";
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
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "admin", reason, reasonCode: "sign_in_again" }) }, "/components");
    expect(await screen.findByText("Sign in again to make changes", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(`${reason} You can still view everything.`)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Log out" })).toHaveAttribute("href", "/oauth/sign_out");
    expect(screen.queryByText(/Read-only access/)).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("shows another backend reason without Log out", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "admin", reason: "Changes are paused for maintenance" }) }, "/components");
    expect(await screen.findByText("Changes are paused for maintenance. You can still view everything.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("keeps the read-only notice for a user without the cluster-admin role", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "viewer" }) }, "/components");
    expect(await screen.findByText("Read-only access", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/requires the cluster-admin role/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Sign in again/)).not.toBeInTheDocument();
  });
});
