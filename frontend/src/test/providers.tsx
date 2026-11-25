import React from "react";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RoleAppStateProvider } from "../state/AppState";
import { AppInfoProvider } from "../state/AppInfo";
import { PermissionsProvider } from "../state/Permissions";
import { LiveAnnouncerProvider } from "../state/LiveAnnouncer";
import { ProgressViewsProvider } from "../components/ProgressViews";
import { EnvironmentRunsProvider } from "../components/environment/EnvironmentRunsProvider";
import { getUserPermissions } from "../services/api";
import type { DashboardState, OperationStatusResponse, StatusResponse, UpdateCheck, UserOperationStatus, UserPermissions, VersionInfo } from "../types";
import { contract } from "./contract";

/** A healthy nightly install: rhoai-3.6, newer build available. */
export function nightlyStatus(overrides: Partial<StatusResponse> = {}): StatusResponse {
  return {
    cluster: { server: "s", version: "4.22.8", user: "me" },
    subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "stable-3.x", state: "AtLatestKnown" },
    csv: { name: "rhods-operator.3.6.0", version: "3.6.0", phase: "Succeeded" },
    catalogSource: { exists: true, name: "rhoai-catalog-dev", image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "4eff06d6".padEnd(64, "0"), state: "READY" },
    pullSecret: { exists: true, valid: true },
    imageMirror: { exists: true, name: "m", source: "registry.redhat.io/rhoai" },
    stableSource: "redhat-operators",
    stableChannel: "stable-3.x",
    stableVersion: "3.5.1",
    dscExists: true,
    nightly: {
      installed: {
        image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "4eff06d6".padEnd(64, "0"),
        tag: "rhoai-3.6",
        digest: "sha256:" + "4eff06d6".padEnd(64, "0"),
        buildDate: "2026-10-01T12:02:38Z",
        dashboardCommit: "32a213123fbc434a602ba5b05e4a2022dd0d85d6",
        dashboardGitURL: "https://github.com/red-hat-data-services/odh-dashboard",
      },
      latest: {
        image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "0b3f9b4d".padEnd(64, "1"),
        tag: "rhoai-3.6",
        digest: "sha256:" + "0b3f9b4d".padEnd(64, "1"),
        buildDate: "2026-10-05T21:47:53Z",
        dashboardCommit: "a3b6b581b465f97f8701f220efadcf8787879dde",
        dashboardGitURL: "https://github.com/red-hat-data-services/odh-dashboard",
      },
      updateAvailable: true,
    },
    ...overrides,
  };
}

export const IDLE_OPERATION: OperationStatusResponse = { inProgress: false, operation: null };

/**
 * The app-wide reads, as the providers call them: with the AbortSignal of
 * their request (aborted when a newer read of the same kind starts or the
 * app unmounts), which renderWithApp passes on.
 */
export interface AppFetchers {
  status?: (signal?: AbortSignal) => Promise<StatusResponse>;
  operation?: (signal?: AbortSignal) => Promise<OperationStatusResponse | UserOperationStatus>;
  permissions?: (signal?: AbortSignal) => Promise<UserPermissions>;
  version?: (signal?: AbortSignal) => Promise<VersionInfo>;
  update?: (signal?: AbortSignal) => Promise<UpdateCheck>;
  dashboard?: (signal?: AbortSignal) => Promise<DashboardState>;
}

/** A cluster-admin's permission answer (the default), from the generated contract (./contract.ts). */
export const ADMIN_PERMISSIONS: UserPermissions = { ...contract.permissionsAdmin(), user: "me" };
/** A user who is not a cluster-admin (per-user views). */
export const USER_PERMISSIONS: UserPermissions = { ...contract.permissionsUser(), user: "dev-user" };

interface RenderedApp {
  /** App-wide reads in flight. */
  pending: Set<Promise<unknown>>;
  /** A read started since the last settled() finished. */
  unsettled: boolean;
  /** The reads that answered while a waitFor or findBy waited (see renderWithApp). */
  answeredOutsideAct: Set<string>;
  settled: () => Promise<void>;
}

// The apps this test rendered (renderWithApp), for settleApps and setup.ts.
let renderedApps: RenderedApp[] = [];

/**
 * Waits until every app this test rendered is settled (see renderWithApp's
 * settled()). A test that renders the app ends with this.
 */
export async function settleApps(): Promise<void> {
  for (const app of renderedApps) await app.settled();
}

/**
 * What the apps this test rendered did outside act, and forget the apps.
 * setup.ts fails such a test (see renderWithApp).
 *
 * - unsettled: apps with reads that started after their last settled():
 *   those answers would update the page after the test.
 * - answeredOutsideAct: the reads that answered while a waitFor or findBy
 *   waited.
 */
export function appsOutsideAct(): { unsettled: number; answeredOutsideAct: string[] } {
  const unsettled = renderedApps.filter((a) => a.unsettled).length;
  const answeredOutsideAct = [...new Set(renderedApps.flatMap((a) => [...a.answeredOutsideAct]))];
  renderedApps = [];
  return { unsettled, answeredOutsideAct };
}

/**
 * False while Testing Library's waitFor or findBy waits: its asyncWrapper
 * turns IS_REACT_ACT_ENVIRONMENT off until one timer after the wait ended
 * (@testing-library/react pure.js), so that React applies updates on its
 * own scheduler instead of warning about them. setup.ts turns it on.
 */
function actEnvironment(): boolean {
  return (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT === true;
}

/**
 * Renders a page inside the app's providers. The permission check goes
 * through the global fetch, so stubApi's `/api/user/permissions` (a
 * cluster-admin by default) decides it; the other app-wide reads are injected.
 */
export function renderPage(ui: React.ReactNode, route = "/", fetchers: AppFetchers = {}) {
  return renderWithApp(ui, { permissions: getUserPermissions, ...fetchers }, route);
}

/**
 * Renders `ui` inside the app's providers with injected backend reads. As
 * in the app, the status is read only once the permission check said
 * cluster-admin (the default answer).
 *
 * The app's reads answer inside act: wait for them with settled() (or
 * settleApps()) before the test waits on the page with waitFor or findBy,
 * and resolve a read the test holds open inside act. A read that answers
 * while a waitFor or findBy waits fails the test (setup.ts): waitFor turns
 * act's checks off while it waits, React then renders the answer on its
 * own scheduler, which yields every 5 ms, and the reads that answer starts
 * (the status once the role is known, the Dashboard Dev state once the
 * version is) finish before or after the wait ended depending on the
 * machine's load. After it, their updates are outside act: React warns
 * "not wrapped in act", in some runs only.
 */
export function renderWithApp(ui: React.ReactNode, fetchers: AppFetchers = {}, route = "/") {
  // Every app-wide read in flight, so settled() can wait for their answers.
  const pending = new Set<Promise<unknown>>();
  const app: RenderedApp = { pending, unsettled: false, answeredOutsideAct: new Set(), settled: async () => {} };
  renderedApps.push(app);
  const track = <T,>(name: string, read: (signal?: AbortSignal) => Promise<T>) => (signal?: AbortSignal): Promise<T> => {
    const p = read(signal);
    app.unsettled = true;
    pending.add(p);
    const done = () => {
      pending.delete(p);
      // The provider's own continuation runs right after this one, in the
      // same microtask checkpoint: with the same act environment.
      if (!actEnvironment()) app.answeredOutsideAct.add(name);
    };
    p.then(done, done);
    return p;
  };
  const status = track("status", fetchers.status ?? (async () => nightlyStatus()));
  const operation = track("operation", fetchers.operation ?? (async () => IDLE_OPERATION));
  const permissions = track("permissions", fetchers.permissions ?? (async () => ADMIN_PERMISSIONS));
  const version = track("version", fetchers.version ?? (async () => ({ version: "dev", commit: "unknown", buildDate: "unknown", templateOutdated: false })));
  const update = track("update check", fetchers.update ?? (async () => ({ updateAvailable: false, majorUpgrade: false })));
  const dashboard = track("Dashboard Dev state", fetchers.dashboard ?? (async () => ({ currentImage: "", isCustomPR: false, managed: true, podStatus: "", podReady: true, containersReady: 1, containersTotal: 1, rolloutPending: false, canAssistRollout: false, override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false } } as DashboardState)));
  /**
   * Waits, inside act, until the app-wide reads (permissions, status,
   * operation, version, update check, dashboard state) answered and React
   * rendered what they changed, including the reads those answers start
   * (the status once the role is known). Assert on the page after this, not
   * on one whose providers are still loading. Not for a test that holds a
   * read open (a deferred it resolves later).
   */
  const settled = async () => {
    for (let round = 0; round < 20; round++) {
      await act(async () => {
        await Promise.allSettled([...pending]);
      });
      if (pending.size === 0) {
        app.unsettled = false;
        return;
      }
    }
    throw new Error("the app's reads did not settle: a read started after every answer (a poll without delay?)");
  };
  app.settled = settled;
  const result = render(
    <LiveAnnouncerProvider>
      <MemoryRouter initialEntries={[route]}>
        <PermissionsProvider fetchPermissions={permissions}>
          <RoleAppStateProvider fetchStatus={status} fetchOperation={operation}>
            <AppInfoProvider fetchVersion={version} fetchUpdateCheck={update} fetchDashboardState={dashboard}>
              <ProgressViewsProvider><EnvironmentRunsProvider>{ui}</EnvironmentRunsProvider></ProgressViewsProvider>
            </AppInfoProvider>
          </RoleAppStateProvider>
        </PermissionsProvider>
      </MemoryRouter>
    </LiveAnnouncerProvider>,
  );
  return { ...result, settled };
}
