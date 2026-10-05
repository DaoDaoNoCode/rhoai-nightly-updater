import React from "react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppStateProvider } from "../state/AppState";
import { AppInfoProvider } from "../state/AppInfo";
import { LiveAnnouncerProvider } from "../state/LiveAnnouncer";
import type { DashboardState, OperationStatusResponse, StatusResponse, UserPermissions, VersionInfo } from "../types";

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

export interface AppFetchers {
  status?: () => Promise<StatusResponse>;
  operation?: () => Promise<OperationStatusResponse>;
  permissions?: () => Promise<UserPermissions>;
  version?: () => Promise<VersionInfo>;
  dashboard?: () => Promise<DashboardState>;
}

/** Renders `ui` inside the app's providers with injected backend reads. */
export function renderWithApp(ui: React.ReactNode, fetchers: AppFetchers = {}, route = "/") {
  const status = fetchers.status ?? (async () => nightlyStatus());
  const operation = fetchers.operation ?? (async () => IDLE_OPERATION);
  const permissions = fetchers.permissions ?? (async () => ({ canMutate: true, user: "me" }));
  const version = fetchers.version ?? (async () => ({ version: "dev", commit: "unknown", buildDate: "unknown", templateOutdated: false }));
  const dashboard = fetchers.dashboard ?? (async () => ({ currentImage: "", isCustomPR: false, managed: true, podStatus: "", podReady: true, containersReady: 1, containersTotal: 1, rolloutPending: false, canAssistRollout: false, override: { active: false, operatorPaused: false, sessionRecorded: false, stale: false, dashboardDeleting: false } } as DashboardState));
  return render(
    <LiveAnnouncerProvider>
      <MemoryRouter initialEntries={[route]}>
        <AppStateProvider fetchStatus={status} fetchOperation={operation}>
          <AppInfoProvider fetchPermissions={permissions} fetchVersion={version} fetchDashboardState={dashboard}>
            {ui}
          </AppInfoProvider>
        </AppStateProvider>
      </MemoryRouter>
    </LiveAnnouncerProvider>,
  );
}
