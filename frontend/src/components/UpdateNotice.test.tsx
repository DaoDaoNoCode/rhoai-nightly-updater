import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { GlobalBanners } from "./GlobalBanners";
import { HelpButton } from "./HelpModal";
import { renderWithApp } from "../test/providers";
import { isReleaseVersion } from "../utils";
import type { UpdateCheck, VersionInfo } from "../types";

const release = (version: string): (() => Promise<VersionInfo>) =>
  async () => ({ version, commit: "c".repeat(40), buildDate: "d", templateRevision: "4", expectedTemplateRevision: "4", templateOutdated: false });

const MAJOR: UpdateCheck = {
  current: "v1.0.0", latest: "v2.0.0", updateAvailable: true, majorUpgrade: true,
  releaseNotesURL: "https://gitlab.example.com/group/app/-/releases/v2.0.0",
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
  afterEach(() => localStorage.clear());

  it("asks for a full redeploy for a new major version, with the release notes", async () => {
    renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => MAJOR });
    expect(await screen.findByText("v2.0.0 is available")).toBeInTheDocument();
    expect(screen.getByText(/requires a full redeploy/)).toBeInTheDocument();
    expect(screen.getByText("git checkout v2.0.0 && make upgrade")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Release notes" })).toHaveAttribute("href", MAJOR.releaseNotesURL);
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

    const again = renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => MAJOR });
    await waitFor(() => expect(screen.queryByText("v2.0.0 is available")).not.toBeInTheDocument());
    again.unmount();

    renderWithApp(<GlobalBanners />, { version: release("v1.0.0"), update: async () => ({ ...MAJOR, latest: "v2.0.1" }) });
    expect(await screen.findByText("v2.0.1 is available")).toBeInTheDocument();
  });

  it("does not check for main or commit builds", async () => {
    const update = vi.fn(async () => MAJOR);
    renderWithApp(<GlobalBanners />, { version: release("2cdcb42d"), update });
    await new Promise((r) => setTimeout(r, 20));
    expect(update).not.toHaveBeenCalled();
    expect(screen.queryByText(/is available/)).not.toBeInTheDocument();
  });

  it("shows nothing when the running release is current", async () => {
    renderWithApp(<GlobalBanners />, { version: release("v2.0.0"), update: async () => ({ current: "v2.0.0", latest: "v2.0.0", updateAvailable: false, majorUpgrade: false }) });
    await new Promise((r) => setTimeout(r, 20));
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
    renderWithApp(<HelpButton />, { version: release("v1.0.0"), update: async () => MAJOR });
    await new Promise((r) => setTimeout(r, 20));
    fireEvent.click(screen.getByRole("button", { name: /help/i }));
    expect(await screen.findByText("Newer release")).toBeInTheDocument();
    expect(screen.getByText(/a new major version: a full redeploy/)).toBeInTheDocument();
  });
});
