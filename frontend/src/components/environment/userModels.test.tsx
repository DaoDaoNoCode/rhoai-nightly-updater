import { describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { UserProjectView } from "./UserProjectView";
import { CLUSTER_ADMIN_MODELS } from "./DeployProjectModel";
import { projectStatus, stubUserApi } from "../../test/userFixtures";
import { jsonResponse } from "../../test/apiStub";
import type { CatalogModel, ModelExposures } from "../../types";

// A user's models in one of their projects (non-admin design B6): Deploy a
// model reached only inside the cluster, without token authentication;
// Delete of the tool's model. Through the inference gateway or with token
// authentication a model is a cluster-admin's (the Models tab).

const CATALOG: CatalogModel[] = [
  { name: "smollm2-135m-instruct", displayName: "SmolLM2 135M Instruct (CPU)", uri: "hf://x", cpu: "2", memoryRequest: "4Gi", memory: "5Gi", type: "chat", useCase: "Chat (smoke test)", description: "", args: "", downloadSize: "270 MB" },
  { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://y", cpu: "4", memoryRequest: "6Gi", memory: "8Gi", type: "chat", useCase: "Chat", description: "", args: "", downloadSize: "1.0 GB", default: true },
];

function exposures(capacity: "fits" | "may-not-fit" = "fits"): ModelExposures {
  return {
    project: "team-a", model: "qwen25-05b-instruct", default: "internal", authReady: false, capacity,
    options: [
      { key: "internal", exposure: "internal", title: "Only inside the cluster", description: "No outside address.", available: true, warning: "Calls to the in-cluster address are not authenticated." },
    ],
  };
}

function stub(capacity: "fits" | "may-not-fit" = "fits") {
  return stubUserApi({
    "/api/resources/status": () => jsonResponse({ ...projectStatus("team-a"), modelCatalog: CATALOG }),
    "/api/resources/models/exposures": () => jsonResponse(exposures(capacity)),
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
  it("deploys inside the cluster without token authentication, and says who deploys the rest", async () => {
    const api = stub();
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const dialog = await openDeploy();
    expect(await within(dialog).findByText("Calls to the in-cluster address are not authenticated.")).toBeInTheDocument();
    expect(within(dialog).getByText(CLUSTER_ADMIN_MODELS)).toBeInTheDocument();
    expect(within(dialog).queryByRole("radio")).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("checkbox", { name: "Require token authentication" })).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Deploy" }));
    await waitFor(() => expect(api.bodies["POST /api/resources/models/project/stream"]).toEqual([{ action: "setup", project: "team-a", model: "qwen25-05b-instruct", exposure: "internal" }]));
  });

  it("warns when the cluster may not have room for the model", async () => {
    stub("may-not-fit");
    render(<UserProjectView project="team-a" refreshKey={0} />);
    const dialog = await openDeploy();
    expect(await within(dialog).findByText("The cluster may not have room for it")).toBeInTheDocument();
  });

  it("deletes the tool's model after a confirmation", async () => {
    const api = stub();
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
