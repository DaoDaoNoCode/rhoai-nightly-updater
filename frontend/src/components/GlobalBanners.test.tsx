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
