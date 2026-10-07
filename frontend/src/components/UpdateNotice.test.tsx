import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { HelpButton } from "./HelpModal";
import { renderWithApp } from "../test/providers";
import { useUpdateCheck, useVersion } from "../state/AppInfo";
import { UPDATE_CHECK_REFRESH_MS } from "../constants";
import { isReleaseVersion } from "../utils";
import type { UpdateCheck, VersionInfo } from "../types";

const release = (version: string): (() => Promise<VersionInfo>) =>
  async () => ({ version, commit: "c".repeat(40), buildDate: "d", templateRevision: "4", expectedTemplateRevision: "4", templateOutdated: false });

const MAJOR: UpdateCheck = {
  current: "v1.0.0", latest: "v2.0.0", updateAvailable: true, majorUpgrade: true,
  releaseNotesURL: "https://gitlab.example.com/group/app/-/releases/v2.0.0",
};

/** Shows what AppInfo loaded, so tests wait on state instead of sleeping. */
const Probe: React.FC = () => {
  const version = useVersion();
  const update = useUpdateCheck();
  return <span data-testid="probe">{`${version?.version ?? "-"}|${update?.latest ?? "-"}`}</span>;
};

describe("isReleaseVersion", () => {
  it("accepts vMAJOR.MINOR.PATCH only", () => {
    expect(isReleaseVersion("v2.0.0")).toBe(true);
    expect(isReleaseVersion("v10.2.33")).toBe(true);
    for (const v of ["2cdcb42d", "dev", "main-2cdcb42d", "v2.0", "2.0.0", "v2.0.0-rc.1", "v02.0.0", undefined]) {
      expect(isReleaseVersion(v)).toBe(false);
    }
  });
});

describe("update available notice", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => {
    localStorage.clear();
    vi.useRealTimers();
  });

  it("asks for a full redeploy for a new major version, with the release notes", async () => {
    renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => MAJOR });
    expect(await screen.findByText("v2.0.0 is available")).toBeInTheDocument();
    expect(screen.getByText(/requires a full redeploy/)).toBeInTheDocument();
    expect(screen.getByText("git checkout v2.0.0 && make upgrade")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Release notes" })).toHaveAttribute("href", MAJOR.releaseNotesURL);
  });

  it("offers the release's no-clone installer as a copyable command, with the git route second", async () => {
    const installerURL = "https://github.com/example/app/releases/download/v2.0.1/install.sh";
    renderWithApp(<GlobalBanners />, {
      version: release("v2.0.0"),
      update: async () => ({
        current: "v2.0.0", latest: "v2.0.1", updateAvailable: true, majorUpgrade: false,
        releaseNotesURL: "https://github.com/example/app/releases/v2.0.1", installerURL,
      }),
    });
    expect(await screen.findByText("v2.0.1 is available")).toBeInTheDocument();
    expect(screen.getByText(/An admin upgrades with the v2.0.1 installer/)).toBeInTheDocument();
    expect(screen.getByText(`curl -fsSLO '${installerURL}' && less install.sh && bash install.sh`)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy the v2.0.1 install command" })).toBeInTheDocument();
    expect(screen.getByText("git checkout v2.0.1 && make upgrade")).toBeInTheDocument();
    expect(screen.queryByText(/full redeploy/)).not.toBeInTheDocument();
    // A GitHub asset downloads anonymously: no sign-in hint.
    expect(screen.queryByText(/asks you to sign in/)).not.toBeInTheDocument();
  });

  it("keeps the full-redeploy wording for a major release with an installer", async () => {
    const installerURL = "https://gitlab.example.com/group/app/-/releases/v3.0.0/downloads/install.sh";
    renderWithApp(<GlobalBanners />, {
      version: release("v2.0.1"),
      update: async () => ({ current: "v2.0.1", latest: "v3.0.0", updateAvailable: true, majorUpgrade: true, installerURL }),
    });
    expect(await screen.findByText(/v3.0.0 is a new major version and requires a full redeploy/)).toBeInTheDocument();
    expect(screen.getByText(/An admin redeploys with the v3.0.0 installer/)).toBeInTheDocument();
    expect(screen.getByText(`curl -fsSLO '${installerURL}' && less install.sh && bash install.sh`)).toBeInTheDocument();
    // A GitLab release asset may need a sign-in.
    expect(screen.getByText(/asks you to sign in/)).toBeInTheDocument();
  });

  it("names the upgrade command for a minor release, without a redeploy warning", async () => {
    renderWithApp(<GlobalBanners />, {
      version: release("v2.0.0"),
      update: async () => ({ current: "v2.0.0", latest: "v2.1.0", updateAvailable: true, majorUpgrade: false }),
    });
    expect(await screen.findByText("v2.1.0 is available")).toBeInTheDocument();
    expect(screen.queryByText(/full redeploy/)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Release notes" })).not.toBeInTheDocument();
  });

  it("stays dismissed for that release, and comes back for a newer one", async () => {
    const { unmount } = renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => MAJOR });
    fireEvent.click(await screen.findByRole("button", { name: /Close Info alert: .*v2.0.0 is available/ }));
    expect(screen.queryByText("v2.0.0 is available")).not.toBeInTheDocument();
    unmount();

    const again = renderWithApp(<><GlobalBanners /><Probe /></>, { version: release("v1.0.0"), update: async () => MAJOR });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("v1.0.0|v2.0.0"));
    expect(screen.queryByText("v2.0.0 is available")).not.toBeInTheDocument();
    again.unmount();

    renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => ({ ...MAJOR, latest: "v2.0.1" }) });
    expect(await screen.findByText("v2.0.1 is available")).toBeInTheDocument();
  });

  it("does not check for main or commit builds", async () => {
    const update = vi.fn(async () => MAJOR);
    renderWithApp(<><GlobalBanners /><Probe /></>, { version: release("2cdcb42d"), update });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("2cdcb42d|-"));
    expect(update).not.toHaveBeenCalled();
    expect(screen.queryByText(/is available/)).not.toBeInTheDocument();
  });

  it("shows nothing when the running release is current", async () => {
    renderWithApp(<><GlobalBanners /><Probe /></>, {
      version: release("v2.0.0"),
      update: async () => ({ current: "v2.0.0", latest: "v2.0.0", updateAvailable: false, majorUpgrade: false }),
    });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("v2.0.0|v2.0.0"));
    expect(screen.queryByText(/is available/)).not.toBeInTheDocument();
  });

  it("keeps the template-outdated notice next to it", async () => {
    renderWithApp(<GlobalBanners />, {
      version: async () => ({ version: "v1.0.0", commit: "x", buildDate: "d", templateRevision: "", expectedTemplateRevision: "2", templateOutdated: true }),
      update: async () => MAJOR,
    });
    expect(await screen.findByText(/this updater's deployment is out of date/)).toBeInTheDocument();
    expect(await screen.findByText("v2.0.0 is available")).toBeInTheDocument();
  });

  it("lists the newer release in the help dialog", async () => {
    renderWithApp(<><HelpButton /><Probe /></>, { version: release("v1.0.0"), update: async () => MAJOR });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("v1.0.0|v2.0.0"));
    fireEvent.click(screen.getByRole("button", { name: /help/i }));
    expect(await screen.findByText("Newer release")).toBeInTheDocument();
    expect(screen.getByText(/a new major version: a full redeploy/)).toBeInTheDocument();
  });

  it("checks again on window focus, at most every six hours", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-06T08:00:00Z"));
    let latest = "v2.0.0";
    const update = vi.fn(async () => ({ ...MAJOR, latest }));
    renderWithApp(<><GlobalBanners /><Probe /></>, { version: release("v1.0.0"), update });
    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("v1.0.0|v2.0.0"));
    expect(update).toHaveBeenCalledTimes(1);

    vi.setSystemTime(new Date(Date.now() + UPDATE_CHECK_REFRESH_MS - 60_000));
    act(() => { window.dispatchEvent(new Event("focus")); });
    expect(update).toHaveBeenCalledTimes(1);

    latest = "v2.1.0";
    vi.setSystemTime(new Date(Date.now() + 120_000));
    act(() => { window.dispatchEvent(new Event("focus")); });
    expect(update).toHaveBeenCalledTimes(2);
    expect(await screen.findByText("v2.1.0 is available")).toBeInTheDocument();
  });
});
