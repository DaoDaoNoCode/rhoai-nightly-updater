import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { CatalogModel, InferenceGatewayState, ModelState, ResourcesStatus } from "../../types";
import { IDLE, modelsState } from "./featureState";
import { UseModel, agoWords, gatewayListRelease, modelsConfirm } from "./ModelsTab";

const gw: InferenceGatewayState = {
  deployed: true, ready: true, managedByTool: true, namespace: "openshift-ingress", name: "openshift-ai-inference", message: "Running",
  allowedProjects: ["openshift-ingress", "dev"], addedProjects: ["dev"], usedBy: ["dev/qwen25-05b-instruct"],
};
const qwen: CatalogModel = { name: "qwen25-05b-instruct", displayName: "Qwen2.5 0.5B Instruct (CPU)", uri: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae5576", cpu: "4", memoryRequest: "6Gi", memory: "8Gi",
  type: "chat", useCase: "Chat", description: "A small chat model.", args: "--max-model-len 4096", downloadSize: "1.0 GB", default: true };
const model = (over: Partial<ModelState> = {}): ModelState => ({
  deployed: true, ready: true, managedByTool: true, namespace: "dev", name: "qwen25-05b-instruct", message: "Running",
  displayName: "Qwen2.5 0.5B Instruct (CPU)", url: "https://inference.apps.example.com/dev/qwen25-05b-instruct", ...over,
});
const status = (gateway: InferenceGatewayState, items: ModelState[] = [model()]): ResourcesStatus =>
  ({ pipelineServers: [], inferenceGateway: gateway, models: { catalog: [qwen], items } }) as unknown as ResourcesStatus;

describe("Models review fixes", () => {
  it("deleting a project's last model on the gateway says the project goes off its list", () => {
    const st = status(gw);
    expect(gatewayListRelease(st, "dev", ["qwen25-05b-instruct"])).toBe(true);
    const spec = modelsConfirm({ kind: "teardown-model", project: "dev", model: "qwen25-05b-instruct" }, st)!;
    render(<>{spec.changes.map((c, i) => <div key={i}>{c}</div>)}</>);
    expect(screen.getByText(/is taken off the gateway's projects; routes made by hand there stop attaching\./)).toBeInTheDocument();
    // Another model there keeps it; a project the tool did not add is never taken off.
    expect(gatewayListRelease(status({ ...gw, usedBy: ["dev/qwen25-05b-instruct", "dev/granite"] }), "dev", ["qwen25-05b-instruct"])).toBe(false);
    expect(gatewayListRelease(status({ ...gw, addedProjects: [] }), "dev", ["qwen25-05b-instruct"])).toBe(false);
  });

  it("deleting a model set up through MaaS only says it is unpublished first", () => {
    const st = status(gw, [model({ exposure: "maas", maasPublished: true, maasOnlySetup: true })]);
    const spec = modelsConfirm({ kind: "teardown-model", project: "dev", model: "qwen25-05b-instruct", unpublish: true }, st)!;
    render(<>{spec.summary}{spec.changes.map((c, i) => <div key={i}>{c}</div>)}</>);
    expect(screen.getByText(/Unpublishes it from Models as a Service first \(its subscription entry and model reference go\)\./)).toBeInTheDocument();
    expect(screen.getByText(/which was set up to be reached through MaaS only, so it is unpublished first/)).toBeInTheDocument();
  });

  it("deleting any other model says nothing about MaaS", () => {
    const spec = modelsConfirm({ kind: "teardown-model", project: "dev", model: "qwen25-05b-instruct" }, status(gw))!;
    render(<>{spec.summary}{spec.changes.map((c, i) => <div key={i}>{c}</div>)}</>);
    expect(screen.queryByText(/Models as a Service/)).not.toBeInTheDocument();
    expect(screen.queryByText(/unpublished first/)).not.toBeInTheDocument();
  });

  it("a model set up through MaaS only has no direct address to get back", () => {
    const m = model({ exposure: "maas", maasPublished: true, maasOnlySetup: true, servedName: "qwen25-05b-instruct" });
    render(<MemoryRouter><UseModel model={m} catalog={qwen} project="dev" onMaaS={() => {}} readOnly={false} baseReason={null} onSetUpAgain={() => {}} /></MemoryRouter>);
    expect(screen.getByText(/it has no direct address\./)).toBeInTheDocument();
    expect(screen.getByText(/leaves it with no outside route, as it was set up/)).toBeInTheDocument();
    expect(screen.queryByText(/To call its direct address again/)).not.toBeInTheDocument();
    expect(screen.queryByText(/answers 404 until it is unpublished/)).not.toBeInTheDocument();
  });

  it("a model reached only inside the cluster has its own next steps", () => {
    const m = model({ exposure: "internal", internalUrl: "https://qwen25-05b-instruct-kserve-workload-svc.dev.svc.cluster.local", servedName: "qwen25-05b-instruct" });
    render(<MemoryRouter><UseModel model={m} catalog={qwen} project="dev" onMaaS={() => {}} readOnly={false} baseReason={null} onSetUpAgain={() => {}} /></MemoryRouter>);
    expect(screen.getByText(/Run the command above from a pod on the cluster \(for example a workbench in dev\)/)).toBeInTheDocument();
    expect(screen.getByText(/No token is needed\./)).toBeInTheDocument();
    expect(screen.getByText("To reach it from outside the cluster, delete it and set it up again through a gateway.")).toBeInTheDocument();
    expect(screen.queryByText(/with the token as the API key/)).not.toBeInTheDocument();
  });

  it("a check's time reads as how long ago", () => {
    const now = Date.parse("2026-11-18T18:10:00Z");
    expect(agoWords("2026-11-18T18:08:00Z", now)).toBe("2 minutes ago");
    expect(agoWords("2026-11-18T18:09:50Z", now)).toBe("just now");
    expect(agoWords("2026-11-18T15:10:00Z", now)).toBe("3 hours ago");
  });

  it("a model that answers without a token needs attention", () => {
    const st = status(gw, [model({ callCheck: { result: "unauthenticated", ok: false, message: "Answers without a token.", at: "2026-11-18T18:00:00Z" } })]);
    expect(modelsState(st, "dev", [], IDLE, "qwen25-05b-instruct").status).toBe("needs-attention");
  });
});
