import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { EnvironmentPage } from "./EnvironmentPage";
import type { ResourcesStatus, ResourceState } from "../types";
import { jsonResponse } from "../test/utils";

vi.mock("../state/AppInfo", () => ({
  useMutationBlocker: () => null,
  useClusterBusyHandler: () => () => {},
}));
vi.mock("../components/GlobalBanners", () => ({ GlobalBanners: () => null }));

const notDeployed: ResourceState = { deployed: false, ready: false, message: "Not deployed" };

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Environment page", () => {
  it("is titled Environment and groups storage, pipeline servers and MLflow under Data & pipelines", async () => {
    const status: ResourcesStatus = { minio: notDeployed, mlflow: notDeployed, pipelineServers: [] };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => jsonResponse(url.includes("/api/resources/projects") ? { projects: [] } : status)));
    render(<EnvironmentPage />);

    expect(screen.getByRole("heading", { level: 1, name: "Environment" })).toBeInTheDocument();
    expect(document.title).toBe("Environment — RHOAI Nightly Updater");
    expect(screen.getByText(/^One-click setups for developing and reproducing issues on this cluster/)).toBeInTheDocument();

    const section = screen.getByRole("region", { name: "Data & pipelines" });
    expect(within(section).getByRole("heading", { level: 2, name: "Data & pipelines" })).toBeInTheDocument();
    expect(within(section).getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(["Storage", "Pipeline servers", "MLflow"]);
    expect(await within(section).findByRole("list", { name: "Storage" })).toBeInTheDocument();
  });
});
