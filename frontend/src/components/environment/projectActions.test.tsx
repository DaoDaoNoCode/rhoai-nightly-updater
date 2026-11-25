import React from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";

import { UserProjectView } from "./UserProjectView";
import { CAPABILITY_CHECKING, CAPABILITY_UNKNOWN, ProjectCapabilitiesProvider, useProjectCapability } from "./projectActions";
import { stubUserApi } from "../../test/userFixtures";
import { jsonResponse } from "../../test/apiStub";
import { contract } from "../../test/contract";

// The project actions of a user who is not a cluster-admin (non-admin
// design B3): each control follows GET /api/resources/projects/
// capabilities for its own action; one the user may not use is disabled
// with the backend's reason.

const wrapper = (project: string) => ({ children }: { children: React.ReactNode }) => (
  <ProjectCapabilitiesProvider project={project} refreshKey={0}>{children}</ProjectCapabilitiesProvider>
);

describe("useProjectCapability", () => {
  it("is allowed only when the backend says so, and gives its reason otherwise", async () => {
    stubUserApi();
    const { result } = renderHook(() => ({
      setup: useProjectCapability("team-a", "workbench.setup"),
      remove: useProjectCapability("team-a", "mlflow-samples.remove"),
      regenerate: useProjectCapability("team-a", "mlflow-samples.regenerate"),
      unknown: useProjectCapability("team-a", "models.deploy"),
    }), { wrapper: wrapper("team-a") });
    // Fails closed while the answer loads.
    expect(result.current.setup).toEqual({ allowed: false, reason: CAPABILITY_CHECKING });
    await waitFor(() => expect(result.current.setup.allowed).toBe(true));
    expect(result.current.setup.reason).toBeNull();
    expect(result.current.remove.allowed).toBe(false);
    expect(result.current.remove.reason).toMatch(/ask an admin of project team-a to give you the edit role there/);
    expect(result.current.regenerate.reason).toBe("Regenerate the MLflow demo is done by a cluster-admin in this tool.");
    expect(result.current.unknown).toEqual({ allowed: false, reason: CAPABILITY_UNKNOWN });
  });

  it("fails closed when the check fails, and says when the project is not the user's", async () => {
    stubUserApi({ "/api/resources/projects/capabilities": () => jsonResponse({ error: "Cannot check your permissions in team-a.", errorCode: "authorization_unavailable" }, 503) });
    const failed = renderHook(() => useProjectCapability("team-a", "workbench.setup"), { wrapper: wrapper("team-a") });
    await waitFor(() => expect(failed.result.current.reason).toBe(CAPABILITY_UNKNOWN));
    expect(failed.result.current.allowed).toBe(false);

    stubUserApi({ "/api/resources/projects/capabilities": () => jsonResponse({ error: "Project other is not one of your data science projects.", errorCode: "forbidden_for_user" }, 403) });
    const notMine = renderHook(() => useProjectCapability("other", "workbench.setup"), { wrapper: wrapper("other") });
    await waitFor(() => expect(notMine.result.current.reason).toBe("Project other is not one of your data science projects."));
  });
});

describe("a user's project view", () => {
  it("offers the workbench's actions, and disables Remove of a sample with the reason", async () => {
    const api = stubUserApi();
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const workbench = await screen.findByRole("region", { name: "Workbench" });
    const stop = await within(workbench).findByRole("button", { name: "Stop" });
    await waitFor(() => expect(stop).not.toHaveAttribute("aria-disabled", "true"));
    expect(within(workbench).getByRole("button", { name: "Delete" })).not.toHaveAttribute("aria-disabled", "true");

    const samples = screen.getByRole("region", { name: "MLflow sample data" });
    const remove = await within(samples).findByRole("button", { name: "Remove" });
    await waitFor(() => expect(remove).toHaveAttribute("aria-disabled", "true"));
    expect(within(samples).getByRole("button", { name: "Add" })).not.toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByRole("button", { name: "Regenerate" })).not.toBeInTheDocument();

    // Stop runs the workbench action of this project.
    fireEvent.click(stop);
    await waitFor(() => expect(api.bodies["POST /api/resources/workbench/stream"]).toEqual([{ action: "stop", project: "team-a" }]));
  });

  it("asks before a delete, and sends nothing when it is cancelled", async () => {
    const api = stubUserApi();
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const workbench = await screen.findByRole("region", { name: "Workbench" });
    const del = within(workbench).getByRole("button", { name: "Delete" });
    await waitFor(() => expect(del).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(del);
    const dialog = await screen.findByRole("dialog", { name: /Delete the workbench in team-a/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(api.bodies["POST /api/resources/workbench/stream"]).toBeUndefined();
  });

  it("disables every action while what the user may do is unknown", async () => {
    stubUserApi({ "/api/resources/projects/capabilities": () => jsonResponse({ error: "down", errorCode: "authorization_unavailable" }, 503) });
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const workbench = await screen.findByRole("region", { name: "Workbench" });
    const stop = await within(workbench).findByRole("button", { name: "Stop" });
    await waitFor(() => expect(stop).toHaveAttribute("aria-disabled", "true"));
    expect(contract.capabilitiesUser("team-a").actions["workbench.stop"].allowed).toBe(true);
  });
});
