import React from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { HelpButton } from "./HelpModal";
import { ADMIN_PERMISSIONS, USER_PERMISSIONS, nightlyStatus, renderWithApp, type AppFetchers, settleApps } from "../test/providers";
import { useUpdateCheck, useVersion } from "../state/AppInfo";
import { useRole } from "../state/Permissions";
import type { UpdateCheck, VersionInfo } from "../types";
import { deferred } from "../test/utils";

// A release with a newer major out and an outdated template: everything an
// admin's help dialog can say about upgrading the updater.
const OUTDATED: VersionInfo = {
  version: "v5.0.0", commit: "c".repeat(40), buildDate: "2026-11-20", templateRevision: "7", expectedTemplateRevision: "8", templateOutdated: true,
};
const NEWER: UpdateCheck = {
  current: "v5.0.0", latest: "v6.0.0", updateAvailable: true, majorUpgrade: true, releaseNotesURL: "https://github.com/example/app/releases/v6.0.0",
};

/** Shows what the providers loaded, so a test waits on state instead of sleeping. */
const Probe: React.FC = () => {
  const role = useRole();
  const version = useVersion();
  const update = useUpdateCheck();
  return <span data-testid="probe">{`${role}|${version?.version ?? "-"}|${update?.latest ?? "-"}`}</span>;
};

async function openHelp(fetchers: AppFetchers, settled: string) {
  renderWithApp(<><HelpButton /><Probe /></>, { version: async () => OUTDATED, update: async () => NEWER, ...fetchers });
  if (settled) await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent(settled));
  fireEvent.click(screen.getByRole("button", { name: "Help" }));
  return screen.findByRole("dialog");
}

/** A description list of the dialog, by its label. */
function section(dialog: HTMLElement, label: string): HTMLElement {
  const el = dialog.querySelector<HTMLElement>(`[aria-label="${label}"]`);
  if (!el) throw new Error(`no ${label} in the help dialog`);
  return el;
}

/** Words that name a cluster-admin's pages, actions or upgrade steps. */
const ADMIN_ONLY = [
  /Dashboard Dev/, /Diagnostics/, /\bComponents\b/, /Update to latest/, /Update to a specific build/, /Re-deploy/, /Reinstall/,
  /Install a different build/, /pull secret/i, /image mirror/i, /IDMS/, /S3 storage/, /SeaweedFS/, /seaweedfs/, /oc delete/,
  /make upgrade/, /install\.sh/, /targetRevision/, /Newer release/, /v6\.0\.0/, /full redeploy/, /out of date/,
  /CatalogSource/, /Subscription/, /InstallPlan/, /DSCInitialization/, /OpenShift 4\./,
];

describe("help dialog", () => {
  it("shows a cluster-admin every page, every action and how to upgrade the updater", async () => {
    const dialog = await openHelp({ permissions: async () => ADMIN_PERMISSIONS, status: async () => nightlyStatus() }, "admin|v5.0.0|v6.0.0");
    const help = within(dialog);
    for (const heading of ["Daily update", "If something goes wrong", "Dashboard Dev", "Environment", "One-time cluster setup", "Glossary"]) {
      expect(help.getByRole("heading", { name: heading })).toBeInTheDocument();
    }
    expect(help.getByText("Update to latest")).toBeInTheDocument();
    const glossary = section(dialog, "Glossary");
    for (const term of ["FBC image", "CatalogSource", "Subscription", "InstallPlan", "CSV", "DSC / DSCI"]) {
      expect(within(glossary).getByText(term)).toBeInTheDocument();
    }
    const about = within(section(dialog, "About this installation"));
    expect(await about.findByText("OpenShift 4.22.8, signed in as me")).toBeInTheDocument();
    expect(about.getByText("Newer release")).toBeInTheDocument();
    expect(about.getByText("make upgrade")).toBeInTheDocument();
    expect(about.queryByText("Signed in as")).not.toBeInTheDocument();
    await settleApps();
  });

  it("shows a user who is not a cluster-admin only what they can see and do", async () => {
    const dialog = await openHelp({ permissions: async () => USER_PERMISSIONS }, "user|v5.0.0|v6.0.0");
    const help = within(dialog);
    for (const heading of ["Status", "Build Explorer", "Environment", "While an update runs", "Glossary"]) {
      expect(help.getByRole("heading", { name: heading })).toBeInTheDocument();
    }
    expect(help.getByText(/changes to the cluster are made by a cluster-admin/)).toBeInTheDocument();
    expect(help.getByText(/Your data science projects\. Pick one/)).toBeInTheDocument();
    expect(help.getByRole("link", { name: "#rhoai-build-notifications" })).toBeInTheDocument();
    const terms = [...section(dialog, "Glossary").querySelectorAll("dt")].map((t) => t.textContent);
    expect(terms).toEqual(["FBC image", "CSV", "DSC"]);
    const about = within(section(dialog, "About this installation"));
    expect(about.getByText("Signed in as")).toBeInTheDocument();
    expect(about.getByText("dev-user")).toBeInTheDocument();
    expect(about.getByText("Updater build")).toBeInTheDocument();
    for (const words of ADMIN_ONLY) expect(dialog.textContent).not.toMatch(words);
    await settleApps();
  });

  it("shows nothing for cluster-admins while the permission check has not answered", async () => {
    const answer = deferred<typeof ADMIN_PERMISSIONS>();
    const dialog = await openHelp({ permissions: () => answer.promise }, "checking|v5.0.0|v6.0.0");
    expect(within(dialog).getByRole("heading", { name: "Build Explorer" })).toBeInTheDocument();
    for (const words of ADMIN_ONLY) expect(dialog.textContent).not.toMatch(words);
    // The answer comes in the end, so the test does not end with it open.
    answer.resolve(ADMIN_PERMISSIONS);
    await settleApps();
  });

  it("shows a user's guide when the permission check failed", async () => {
    const dialog = await openHelp({ permissions: async () => { throw new Error("network down"); } }, "user|v5.0.0|v6.0.0");
    expect(within(dialog).getByRole("heading", { name: "While an update runs" })).toBeInTheDocument();
    for (const words of ADMIN_ONLY) expect(dialog.textContent).not.toMatch(words);
    await settleApps();
  });
});
