import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Overview, type OverviewRow } from "./Overview";
import type { FeatureState } from "./featureState";

const UI = "https://rh-ai.apps.example.com/develop-train/pipelines/definitions/dev";

const ready = (project: string, over: Partial<FeatureState> = {}): FeatureState => ({ status: "ready", label: `Ready in ${project}`, next: { kind: "open", label: "Open pipelines", href: UI }, project, ...over });

function renderRows(rows: OverviewRow[]) {
  const handlers = { onSetUp: vi.fn(), onShowIssue: vi.fn(), onOpenTab: vi.fn(), onMigrate: vi.fn(), onSamples: vi.fn() };
  render(
    <MemoryRouter>
      <Overview rows={rows} baseReason={null} setUpReason={null} onCleanup={null} {...handlers} />
    </MemoryRouter>,
  );
  return handlers;
}

const row = (name: string) => screen.getByRole("link", { name }).closest("tr") as HTMLElement;
const cell = (name: string, column: string) => row(name).querySelector(`td[data-label="${column}"]`) as HTMLElement;

describe("Overview table", () => {
  it("an open step is an inline link with the external icon after its text, and does not repeat the project", () => {
    renderRows([{ id: "pipelines", name: "Pipelines", gives: "Run pipelines", state: ready("dev"), href: "?tab=pipelines&project=dev", samples: { label: "Run a sample pipeline", focus: "x" } }]);
    const link = within(cell("Pipelines", "Next step")).getByRole("link", { name: "Open pipelines" });
    expect(link).toHaveAttribute("href", UI);
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveClass("pf-m-link", "pf-m-inline");
    // The icon follows the text on the same line.
    expect(link).toHaveClass("pf-v6-u-text-nowrap");
    const icon = link.querySelector(".pf-v6-c-button__icon");
    expect(icon).toHaveClass("pf-m-end");
    expect(link.lastElementChild).toBe(icon);
    // The second action is a second inline link.
    expect(within(cell("Pipelines", "Next step")).getByRole("button", { name: "Run a sample pipeline" })).toHaveClass("pf-m-inline", "pf-v6-u-text-nowrap");
    expect(cell("Pipelines", "State")).toHaveTextContent("Ready in dev");
  });

  it("ownership is said under the status label, not in it", () => {
    renderRows([{ id: "mlflow", name: "MLflow", gives: "Track experiments", state: { status: "ready", label: "Ready", unmanaged: true, next: null, project: null }, href: "?tab=mlflow" }]);
    const state = cell("MLflow", "State");
    expect(state.querySelector(".pf-v6-c-label")).toHaveTextContent(/^Ready$/);
    expect(within(state).getByText("Not managed by this tool").closest(".pf-v6-c-helper-text")).not.toBeNull();
  });

  it("several projects: the state counts them, a popover lists each with its state and a link to the tab in it, and the next step opens the tab's picker", async () => {
    const projects = [ready("alpha"), ready("beta", { unmanaged: true }), ready("gamma")];
    const h = renderRows([{ id: "pipelines", name: "Pipelines", gives: "Run pipelines", state: { status: "ready", label: "Ready in 3 projects", next: { kind: "choose-project" }, project: null, projects }, href: "?tab=pipelines" }]);
    expect(cell("Pipelines", "State")).toHaveTextContent("Ready in 3 projects");
    fireEvent.click(within(cell("Pipelines", "State")).getByRole("button", { name: "Show projects" }));
    const popover = await screen.findByRole("dialog", { name: "Pipelines in 3 projects" });
    const items = within(within(popover).getByRole("grid", { name: "Pipelines by project" })).getAllByRole("row").slice(1);
    expect(items.map((i) => within(i).getByRole("link").textContent)).toEqual(["alpha", "beta", "gamma"]);
    expect(within(items[1]).getByRole("link")).toHaveAttribute("href", "/?tab=pipelines&project=beta");
    expect(items[1]).toHaveTextContent("Ready");
    expect(items[1]).toHaveTextContent("Not managed by this tool");
    expect(items[0]).not.toHaveTextContent("Not managed");
    fireEvent.click(within(cell("Pipelines", "Next step")).getByRole("button", { name: "Choose a project" }));
    expect(h.onOpenTab).toHaveBeenCalledTimes(1);
  });

  it("a problem among several projects: the next step names the project with the problem and opens its tab there", () => {
    const zeta: FeatureState = { status: "error", label: "Error in zeta", next: { kind: "show-issue", part: "pipeline-server" }, project: "zeta" };
    const state: FeatureState = { ...zeta, label: "Error in 1 of 3 projects", next: { kind: "show-issue", part: "pipeline-server", project: "zeta" }, projects: [ready("alpha"), ready("beta"), zeta] };
    const r: OverviewRow = { id: "pipelines", name: "Pipelines", gives: "Run pipelines", state, href: "?tab=pipelines&project=zeta" };
    const h = renderRows([r]);
    fireEvent.click(within(cell("Pipelines", "Next step")).getByRole("button", { name: "Open zeta" }));
    expect(h.onShowIssue).toHaveBeenCalledWith(r, "pipeline-server");
  });

  it("a problem in the one project: Show issue", () => {
    const state: FeatureState = { status: "error", label: "Error in zeta", next: { kind: "show-issue", part: "pipeline-server" }, project: "zeta" };
    renderRows([{ id: "pipelines", name: "Pipelines", gives: "Run pipelines", state, href: "?tab=pipelines&project=zeta" }]);
    expect(within(cell("Pipelines", "Next step")).getByRole("button", { name: "Show issue" })).toBeInTheDocument();
    expect(within(cell("Pipelines", "State")).queryByRole("button", { name: "Show projects" })).toBeNull();
  });
});
