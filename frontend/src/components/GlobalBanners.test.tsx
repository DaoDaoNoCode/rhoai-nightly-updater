import React from "react";
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { USER_PERMISSIONS, renderWithApp, settleApps } from "../test/providers";
import { ApiError } from "../services/api";
import { formatClockTime } from "../utils";
import type { OperationStatusResponse, ServerOperation, UserOperationStatus } from "../types";
import { contract } from "../test/contract";
import { stubApi } from "../test/apiStub";
import { useRegisterBackgroundRun } from "./ProgressViews";

const remoteUpdate: ServerOperation = {
  id: "op1", type: "update", label: "Update to nightly", user: "qa-user",
  target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "a".repeat(64),
  startedAt: "2026-10-06T14:27:00Z", step: "verify_installplan", stepStatus: "running",
  message: "CSV rhods-operator.3.6.0: Installing (InstallWaiting)", pod: "updater-old", remote: true,
};

describe("the banner of an operation another updater pod runs", () => {
  it("ends the latest progress with a full stop, also after a parenthesis", async () => {
    renderWithApp(<GlobalBanners />, { operation: async () => ({ inProgress: true, operation: remoteUpdate }) }, "/components");
    await settleApps();
    expect(await screen.findByText(/on updater pod updater-old/)).toBeInTheDocument();
    expect(screen.getByText(/Latest progress: CSV rhods-operator\.3\.6\.0: Installing \(InstallWaiting\)\. Another updater pod runs it/)).toBeInTheDocument();
    await settleApps();
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
    await settleApps();
    expect(screen.getByText("Sign in again to make changes", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(`${reason} You can still view everything.`)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Log out" })).toHaveAttribute("href", "/oauth/sign_out");
    expect(screen.queryByText(/Read-only access/)).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("shows another backend reason without Log out", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "admin", role: "admin", reason: "Changes are paused for maintenance" }) }, "/components");
    await settleApps();
    expect(await screen.findByText("Changes are paused for maintenance. You can still view everything.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
    await settleApps();
  });

  it("keeps the read-only notice for a cluster-admin whose answer gives no reason", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => ({ canMutate: false, user: "viewer", role: "admin" }) }, "/components");
    await settleApps();
    expect(await screen.findByText("Read-only access", { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/requires the cluster-admin role/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Log out" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Sign in again/)).not.toBeInTheDocument();
    await settleApps();
  });

  // Per-user views: read-only is what a user is, and their pages already show their view.
  it.each([
    ["role user", { canMutate: false, user: "dev-user", role: "user" as const }],
    ["no role (fail closed)", { canMutate: false, user: "dev-user" }],
  ])("shows a user who is not a cluster-admin no read-only notice (%s)", async (_name, answer) => {
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, { permissions: async () => answer }, "/test-resources");
    await settleApps();
    expect(screen.getByText("loaded")).toBeInTheDocument();
    expect(screen.queryByText(/Read-only access/)).not.toBeInTheDocument();
    expect(screen.queryByText(/cluster-admin role/)).not.toBeInTheDocument();
  });

  it("still warns a user when their permissions could not be checked", async () => {
    renderWithApp(<GlobalBanners />, { permissions: async () => { throw new ApiError({ status: 503, errorCode: "authorization_unavailable", message: "Cannot verify" }); } }, "/");
    await settleApps();
    expect(await screen.findByText("Your permissions could not be checked; changes are disabled", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    await settleApps();
  });
});

describe("the operation banner for a user who is not a cluster-admin", () => {
  const startedAt = "2026-10-06T14:05:00Z";
  const clock = formatClockTime(startedAt);

  it("says when a cluster operation started, never who runs it or what it is", async () => {
    renderWithApp(<GlobalBanners />, {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => ({ running: true, label: "Update to nightly", startedAt }),
    }, "/");
    await settleApps();
    expect(await screen.findByText(`A cluster operation is running since ${clock}. Project actions can start when it ends.`)).toBeInTheDocument();
    expect(screen.queryByText(/qa-user|Someone|on updater pod|View progress|Update to nightly/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
    expect(clock).toMatch(/^\d\d:\d\d$/);
    await settleApps();
  });

  it("shows nothing while no operation runs", async () => {
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => ({ running: false }),
    }, "/");
    await settleApps();
    await screen.findByText("loaded");
    await act(async () => {});
    expect(screen.queryByText(/is running/)).not.toBeInTheDocument();
    await settleApps();
  });
});

describe("notices only a cluster-admin can act on", () => {
  const outdated = async () => ({ version: "v5.0.0", commit: "c", buildDate: "d", templateOutdated: true, templateRevision: "7", expectedTemplateRevision: "8" });
  const newer = async () => ({ updateAvailable: true, majorUpgrade: false, current: "v5.0.0", latest: "v5.1.0" });

  it("shows the deployment and new-release notices to a cluster-admin", async () => {
    renderWithApp(<GlobalBanners />, { version: outdated, update: newer }, "/");
    await settleApps();
    expect(await screen.findByText("For admins: this updater's deployment is out of date", { exact: false })).toBeInTheDocument();
    expect(await screen.findByText("v5.1.0 is available", { exact: false })).toBeInTheDocument();
    await settleApps();
  });

  it("an install from the Helm chart links the published GitOps guide's upgrade section", async () => {
    renderWithApp(<GlobalBanners />, { version: async () => ({ ...(await outdated()), installMethod: "gitops" }), update: newer }, "/");
    await settleApps();
    const deployment = (await screen.findByText("For admins: this updater's deployment is out of date", { exact: false })).closest(".pf-v6-c-alert") as HTMLElement;
    // Its details are folded: open them.
    fireEvent.click(within(deployment).getAllByRole("button").find((b) => b.getAttribute("aria-expanded") === "false")!);
    // Both banners (the deployment's and the new release's), at the running release's tag on the mirror.
    const links = screen.getAllByRole("link", { name: /the GitOps guide: Upgrading and rolling back/ });
    expect(links).toHaveLength(2);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/v5.0.0/docs/GITOPS.md#6-upgrading-and-rolling-back");
      expect(link).toHaveAttribute("target", "_blank");
    }
    expect(screen.queryByText(/docs\/GITOPS\.md/)).not.toBeInTheDocument();
    await settleApps();
  });

  it("an install.sh install links the upgrade guide of the running release, a dev build main's", async () => {
    renderWithApp(<GlobalBanners />, { version: outdated, update: newer }, "/");
    await settleApps();
    const deployment = (await screen.findByText("For admins: this updater's deployment is out of date", { exact: false })).closest(".pf-v6-c-alert") as HTMLElement;
    // Its details are folded: open them.
    fireEvent.click(within(deployment).getAllByRole("button").find((b) => b.getAttribute("aria-expanded") === "false")!);
    expect(screen.getByRole("link", { name: "the upgrade guide" })).toHaveAttribute("href", "https://github.com/DaoDaoNoCode/rhoai-nightly-updater/blob/v5.0.0/docs/UPGRADING.md");
    expect(screen.queryByText(/docs\/UPGRADING\.md/)).not.toBeInTheDocument();
    await settleApps();
  });

  it("does not show them to a user who is not a cluster-admin", async () => {
    const update = vi.fn(newer);
    renderWithApp(<><GlobalBanners /><p>loaded</p></>, { permissions: async () => USER_PERMISSIONS, version: outdated, update }, "/");
    await settleApps();
    await screen.findByText("loaded");
    // The release check itself is allowed for everyone; only the notice is a cluster-admin's.
    await waitFor(() => expect(update).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByText(/deployment is out of date/)).not.toBeInTheDocument();
    expect(screen.queryByText(/is available/)).not.toBeInTheDocument();
    await settleApps();
  });
});

describe("the banners of running project actions", () => {
  const startedAt = "2026-11-20T14:05:00Z";
  const clock = formatClockTime(startedAt);
  const note = "It stops before its next step. Before the storage is created nothing stays; after, the storage stays, recorded as this tool's, and the next setup uses it. No workbench is created.";
  // The generated answer, with the user's own action running in the answering updater pod (cancellable there).
  const withOwnHere = (): UserOperationStatus => {
    const res = contract.operationUserProjectActions();
    res.operations = res.operations?.map((e) => (e.own ? { ...e, cancellable: true, cancelNote: note } : e));
    return res;
  };
  const alertOf = (el: HTMLElement) => el.closest(".pf-v6-c-alert") as HTMLElement;

  it("show a user their own action with Cancel, and another person's in their project without a name", async () => {
    const api = stubApi({ "POST /api/operation/cancel": { cancelRequested: true } });
    let polls = 0;
    renderWithApp(<GlobalBanners />, {
      permissions: async () => USER_PERMISSIONS,
      operation: async () => { polls++; return withOwnHere(); },
    }, "/");
    await settleApps();
    const own = await screen.findByText(`Your action (Set up the workbench) in project team-b is running since ${clock}.`);
    expect(screen.getByText(`An action (Set up the workbench) is running in project team-a since ${clock}.`)).toBeInTheDocument();
    expect(screen.queryByText(/alice|started by|A cluster operation/)).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Cancel" })).toHaveLength(1);

    // Cancel asks first, saying what the action leaves; "Keep it running" sends nothing.
    fireEvent.click(within(alertOf(own)).getByRole("button", { name: "Cancel" }));
    let dialog = await screen.findByRole("dialog", { name: "Cancel Set up the workbench in project team-b?" });
    expect(within(dialog).getByText(note)).toBeInTheDocument();
    expect(within(dialog).getByText("If it finishes before its next step, it is recorded as done.")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep it running" }));
    await settleApps();
    expect(api.bodies["POST /api/operation/cancel"]).toBeUndefined();

    const before = polls;
    fireEvent.click(within(alertOf(own)).getByRole("button", { name: "Cancel" }));
    dialog = await screen.findByRole("dialog", { name: /^Cancel Set up the workbench/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel action" }));
    await settleApps();
    await waitFor(() => expect(api.bodies["POST /api/operation/cancel"]).toEqual([{ id: "op-own" }]));
    await waitFor(() => expect(polls).toBeGreaterThan(before));
    await settleApps();
  });

  it("show a stopping action without Cancel", async () => {
    const res = withOwnHere();
    res.operations = res.operations?.filter((e) => e.own).map((e) => ({ ...e, cancellable: false, cancelling: true }));
    renderWithApp(<GlobalBanners />, { permissions: async () => USER_PERMISSIONS, operation: async () => res }, "/");
    await settleApps();
    expect(await screen.findByText("Cancel requested: it stops before its next step. If it finishes first, it is recorded as done.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("say why a cancel failed, on that action only", async () => {
    stubApi({ "POST /api/operation/cancel": (url: string, init?: RequestInit) => {
      const { id } = JSON.parse(typeof init?.body === "string" ? init.body : "{}") as { id: string };
      return id === "op-a"
        ? new Response(JSON.stringify({ error: "No running action with this ID that you can cancel (it may have finished). Nothing was changed.", errorCode: "not_found" }), { status: 404, headers: { "Content-Type": "application/json" } })
        : new Response(JSON.stringify({ cancelRequested: true }), { status: 202, headers: { "Content-Type": "application/json" } });
    } });
    const action = (id: string, user: string, project: string): ServerOperation => ({
      id, type: "workbench", label: "Set up the workbench", user, startedAt, project, projectUid: "uid-" + project, scope: "project", cancellable: true, cancelNote: note,
    });
    const res: OperationStatusResponse = { inProgress: false, operation: null, operations: [action("op-a", "alice", "team-a"), action("op-b", "bob", "team-b")] };
    renderWithApp(<GlobalBanners />, { operation: async () => res }, "/components");
    await settleApps();
    const a = alertOf(await screen.findByText(/^Set up the workbench in project team-a, started by alice/));
    const b = alertOf(screen.getByText(/^Set up the workbench in project team-b, started by bob/));
    fireEvent.click(within(a).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel action" }));
    await settleApps();
    expect(await within(a).findByText(/No running action with this ID that you can cancel/)).toBeInTheDocument();
    expect(within(b).queryByText(/No running action/)).not.toBeInTheDocument();
    expect(within(b).getByRole("button", { name: "Cancel" })).toBeEnabled();
    await settleApps();
  });

  it("show a cluster-admin every action with who started it, Cancel only where this updater pod may cancel it", async () => {
    const action = (id: string, user: string, project: string, cancellable: boolean): ServerOperation => ({
      id, type: "workbench", label: "Set up the workbench", user, startedAt, project, projectUid: "uid-" + project, scope: "project", cancellable,
    });
    const res: OperationStatusResponse = { inProgress: false, operation: null, operations: [action("op-a", "alice", "team-a", true), action("op-x", "dave", "team-x", false)] };
    renderWithApp(<GlobalBanners />, { operation: async () => res }, "/components");
    await settleApps();
    expect(await screen.findByText(`Set up the workbench in project team-a, started by alice at ${clock}.`)).toBeInTheDocument();
    expect(screen.getByText(`Set up the workbench in project team-x, started by dave at ${clock}.`)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Cancel" })).toHaveLength(1);
    await settleApps();
  });

  it("rank other people's actions below the interrupted notice", async () => {
    const res: OperationStatusResponse = {
      inProgress: false, operation: null,
      interrupted: { type: "update", label: "Update to nightly", user: "qa-user", startedAt, pod: "updater-old" },
      operations: [{ id: "op-x", type: "workbench", label: "Set up the workbench", user: "dave", startedAt, project: "team-x", projectUid: "uid-team-x", scope: "project" }],
    };
    renderWithApp(<GlobalBanners />, { operation: async () => res }, "/components");
    await settleApps();
    await screen.findByText(/^Set up the workbench in project team-x, started by dave/);
    const titles = Array.from(document.querySelectorAll(".pf-v6-c-alert__title")).map((n) => n.textContent ?? "");
    const interrupted = titles.findIndex((t) => t.includes("was interrupted"));
    const others = titles.findIndex((t) => t.includes("started by dave"));
    expect(interrupted).toBeGreaterThanOrEqual(0);
    expect(others).toBeGreaterThan(interrupted);
    await settleApps();
  });

  it("show the tab's own running action once, in its run banner, with Cancel", async () => {
    stubApi({ "POST /api/operation/cancel": { cancelRequested: true } });
    const Run: React.FC = () => {
      const open = React.useCallback(() => {}, []);
      useRegisterBackgroundRun({ title: "Set up the workbench in team-b", startedAt: Date.parse(startedAt), step: null, open });
      return null;
    };
    renderWithApp(<><Run /><GlobalBanners /></>, { permissions: async () => USER_PERMISSIONS, operation: async () => withOwnHere() }, "/");
    await settleApps();
    const run = alertOf(await screen.findByText(/^Set up the workbench in team-b is running/));
    expect(within(run).getByText(/Closing its dialog does not stop it\. Your next project action can start when it ends\./)).toBeInTheDocument();
    expect(within(run).getByRole("button", { name: "Cancel" })).toBeInTheDocument();
    expect(screen.queryByText(/^Your action \(Set up the workbench\)/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Cluster changes from this tool are disabled/)).not.toBeInTheDocument();
    await settleApps();
  });
});
