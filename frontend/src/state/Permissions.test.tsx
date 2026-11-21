import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { usePermissions, useRole, useSignedInUser } from "./Permissions";
import { useClusterStatus, useOperation } from "./AppState";
import { ApiError } from "../services/api";
import { USER_PERMISSIONS, nightlyStatus, renderWithApp, settleApps } from "../test/providers";
import { deferred } from "../test/utils";
import type { UserPermissions } from "../types";

const Probe: React.FC = () => {
  const role = useRole();
  const user = useSignedInUser();
  const { status: perm } = usePermissions();
  const { status, loading } = useClusterStatus();
  const { server } = useOperation();
  return <pre data-testid="probe">{JSON.stringify({ role, user, perm, status: !!status, loading, brief: server.brief ?? null, inProgress: server.inProgress })}</pre>;
};
const probe = () => JSON.parse(screen.getByTestId("probe").textContent || "{}");

describe("the role", () => {
  it.each([
    ["admin", { canMutate: true, user: "me", role: "admin" }, "admin"],
    ["a cluster-admin whose sign-in cannot carry a change", { canMutate: false, user: "me", role: "admin", reason: "Log in again.", reasonCode: "sign_in_again" }, "admin"],
    ["user", { canMutate: false, user: "dev-user", role: "user" }, "user"],
    ["no role (fail closed)", { canMutate: true, user: "me" }, "user"],
    ["an unknown role (fail closed)", { canMutate: true, user: "me", role: "superuser" }, "user"],
  ])("%s", async (_name, answer, role) => {
    renderWithApp(<Probe />, { permissions: async () => answer as UserPermissions });
    await settleApps();
    expect(probe().role).toBe(role);
  });

  it.each([
    ["a JSON 403", () => new ApiError({ status: 403, errorCode: "forbidden", message: "no" })],
    ["503 authorization_unavailable", () => new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot verify" })],
  ])("%s is a user's view (fail closed)", async (_name, err) => {
    renderWithApp(<Probe />, { permissions: async () => { throw err(); } });
    await settleApps();
    expect(probe().perm).not.toBe("checking");
    expect(probe().role).toBe("user");
  });

  it("is 'checking' until the first answer, and the status is not read before it", async () => {
    const answer = deferred<UserPermissions>();
    const status = vi.fn(async () => nightlyStatus());
    renderWithApp(<Probe />, { permissions: () => answer.promise, status });
    await act(async () => {});
    expect(probe()).toMatchObject({ role: "checking", status: false });
    expect(status).not.toHaveBeenCalled();
    await act(async () => { answer.resolve({ canMutate: true, user: "me", role: "admin" }); });
    await settleApps();
    expect(probe()).toMatchObject({ role: "admin", status: true });
    expect(status).toHaveBeenCalledTimes(1);
  });

  it("a user's tab never reads the status", async () => {
    const status = vi.fn(async () => nightlyStatus());
    renderWithApp(<Probe />, { permissions: async () => USER_PERMISSIONS, status });
    await settleApps();
    expect(probe()).toMatchObject({ role: "user", user: "dev-user", loading: false });
    expect(status).not.toHaveBeenCalled();
  });
});

describe("GET /api/operation for a user", () => {
  it("keeps the label and start of a running operation, no names", async () => {
    renderWithApp(<Probe />, { permissions: async () => USER_PERMISSIONS, operation: async () => ({ running: true, label: "Update to nightly", startedAt: "2026-10-06T14:05:00Z" }) });
    await settleApps();
    expect(probe()).toMatchObject({ inProgress: true, brief: { label: "Update to nightly", startedAt: "2026-10-06T14:05:00Z" } });
  });

  it("is idle when nothing runs", async () => {
    renderWithApp(<Probe />, { permissions: async () => USER_PERMISSIONS, operation: async () => ({ running: false }) });
    await settleApps();
    expect(probe().role).toBe("user");
    expect(probe()).toMatchObject({ inProgress: false, brief: null });
  });
});
