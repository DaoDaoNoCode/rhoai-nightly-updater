import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { UserProjectView } from "./UserProjectView";
import { projectStatus, stubUserApi } from "../../test/userFixtures";
import { jsonResponse } from "../../test/apiStub";
import { contract } from "../../test/contract";
import type { CatalogModel, ModelExposures } from "../../types";

// A user's models in one of their projects (non-admin design B6): Deploy
// inside the cluster, or through the inference gateway when it admits
// the project and the user may make token authentication (the project's
// admin role); Delete of the tool's model.

const CATALOG: CatalogModel[] = [
  { name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", uri: "hf://x", cpu: "2", memoryRequest: "4Gi", memory: "5Gi", type: "chat", useCase: "Chat (smoke test)", description: "", args: "", downloadSize: "270 MB" },
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://y", cpu: "4", memoryRequest: "6Gi", memory: "8Gi", type: "chat", useCase: "Chat", description: "", args: "", downloadSize: "1.0 GB", default: true },
];

function exposures(admits: boolean): ModelExposures {
  return {
    project: "team-a", model: "qwen25-05b-instruct", default: admits ? "default-gateway" : "internal", authReady: false,
    gateway: admits ? { admits: true } : { admits: false, problem: "not-admitted" }, capacity: "fits",
    options: [
      { key: "default-gateway", exposure: "default-gateway", title: "Through the cluster's inference gateway", description: "Reachable from outside the cluster.", available: admits,
        reason: admits ? undefined : "The cluster's inference gateway does not admit project team-a. A cluster-admin can allow project team-a on the inference gateway (Models tab, Allow)." },
      { key: "internal", exposure: "internal", title: "Only inside the cluster", description: "No outside address.", available: true, warning: "Calls to the in-cluster address are not authenticated." },
    ],
  };
}

const EDITOR_TOKEN_REASON = "Token authentication creates a Role and a RoleBinding in team-a, which needs the project's admin role. Ask an admin of team-a, or deploy without token authentication.";

function stub(admits: boolean, editor: boolean) {
  return stubUserApi({
    "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), modelCatalog: CATALOG }),
    "/api/resources/models/exposures": () => jsonResponse(exposures(admits)),
    "/api/resources/projects/capabilities": () => {
      const caps = contract.capabilitiesUser("team-a");
      if (editor) caps.actions["models.setup-token"] = { ...caps.actions["models.setup-token"], allowed: false, reason: EDITOR_TOKEN_REASON, code: "missing_permissions", missing: ["x"] };
      return jsonResponse(caps);
    },
  });
}

async function openDeploy() {
  const models = await screen.findByRole("region", { name: "Models" });
  const deploy = await within(models).findByRole("button", { name: "Deploy a model" });
  await waitFor(() => expect(deploy).not.toHaveAttribute("aria-disabled", "true"));
  fireEvent.click(deploy);
  return screen.findByRole("dialog", { name: "Deploy a model in team-a" });
}

describe("a user's models", () => {
  it("lets an editor deploy inside the cluster without token authentication, and says why the rest is not offered", async () => {
    const api = stub(false, true);
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const dialog = await openDeploy();
    const gateway = await within(dialog).findByRole("radio", { name: /Through the cluster's inference gateway/ });
    expect(gateway).toBeDisabled();
    expect(within(dialog).getByText(/A cluster-admin can allow project team-a on the inference gateway/)).toBeInTheDocument();
    const token = within(dialog).getByRole("checkbox", { name: "Require token authentication" });
    expect(token).toBeDisabled();
    expect(within(dialog).getByText(EDITOR_TOKEN_REASON)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Deploy" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/models/project/stream"]).toEqual([{ action: "setup", project: "team-a", model: "qwen25-05b-instruct", exposure: "internal" }]));
  });

  it("lets a project admin deploy through a gateway that admits the project, with token authentication", async () => {
    const api = stub(true, false);
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const dialog = await openDeploy();
    const gateway = await within(dialog).findByRole("radio", { name: /Through the cluster's inference gateway/ });
    await waitFor(() => expect(gateway).not.toBeDisabled());
    fireEvent.click(gateway);
    const token = within(dialog).getByRole("checkbox", { name: "Require token authentication" });
    expect(token).toBeChecked();
    expect(token).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Deploy" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/models/project/stream"]).toEqual([{ action: "setup", project: "team-a", model: "qwen25-05b-instruct", exposure: "default-gateway", tokenAuth: true }]));
  });

  it("deletes the tool's model after a confirmation", async () => {
    const api = stub(false, true);
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const models = await screen.findByRole("region", { name: "Models" });
    const del = await within(models).findByRole("button", { name: "Delete" });
    await waitFor(() => expect(del).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(del);
    const dialog = await screen.findByRole("dialog", { name: /Delete Qwen2.5 0.5B Instruct \(CPU\) in team-a/ });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/models/project/stream"]).toEqual([{ action: "teardown", project: "team-a", model: "qwen25-05b-instruct" }]));
  });
});
