import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QuickResourceCreator } from "../QuickResourceCreator";
import { stubApi } from "../../test/apiStub";
import { contract } from "../../test/contract";
import type { KueueStatus, ResourceState, ResourcesStatus } from "../../types";
import { IDLE } from "./featureState";
import { cleanupCandidates, cleanupSteps } from "./CleanupModal";
import { IMAGE_HELP, OPT_IN_EFFECT, kueueCleanupLines, kueueConfirm, kueueSteps } from "./KueueTab";
import { KUEUE_DEFAULT_IMAGE, kueueState, sampleStatus } from "./kueueState";
import { prerequisitesConfirm } from "./Prerequisites";

const none: ResourceState = { deployed: false, ready: false, message: "Not deployed", managedByTool: false };
const status: ResourcesStatus = { minio: none, mlflow: none, pipelineServers: [], dashboardUrl: "https://rh-ai.apps.example.com" };
const ready = (): KueueStatus => contract.kueue("dev");
const notSetUp = (over: Partial<KueueStatus> = {}): KueueStatus => ({
  ...ready(), objects: ready().objects.map((o) => ({ ...o, present: false, managedByTool: false, active: false })),
  samples: ready().samples.map((s) => ({ kind: s.kind, name: s.name, present: false })), label: { present: false },
  optIn: { profile: { ...ready().optIn.profile, present: false, managedByTool: false } }, ...over,
});

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]}><QuickResourceCreator mutateBlocker={null} /></MemoryRouter>);
}

afterEach(() => vi.unstubAllGlobals());

describe("kueueState", () => {
  it("ready names the project and the samples and opens the dashboard's page", () => {
    expect(kueueState(ready(), "dev", IDLE)).toMatchObject({
      status: "ready", label: "Ready in dev, 1 sample",
      next: { kind: "open", href: "https://rh-ai.apps.example.com/observe-monitor/workload-metrics/workload-status/dev" },
    });
    expect(kueueState(ready(), "dev", { ...IDLE, kueue: { action: "teardown", project: "dev" } })).toMatchObject({ status: "deleting" });
    expect(kueueState(null, "dev", IDLE)).toMatchObject({ loading: true });
  });

  it("not set up offers Set up with the RayCluster sample on and the RayJob off, blocked by the refusal", () => {
    expect(kueueState(notSetUp(), "dev", IDLE).next).toMatchObject({
      kind: "set-up", setup: [{ kind: "setup-kueue", project: "dev", rayCluster: true, rayJob: false, rayImage: KUEUE_DEFAULT_IMAGE }], reason: null,
    });
    const refused = notSetUp({ refusal: "This cluster's Kueue does not serve kueue.x-k8s.io/v1beta2", refusalCode: "unsupported" });
    expect(kueueState(refused, "dev", IDLE).next).toMatchObject({ reason: expect.stringContaining("v1beta2") });
    expect(kueueState(notSetUp({ project: undefined }), null, IDLE)).toMatchObject({ next: { kind: "choose-project" } });
    expect(kueueState(notSetUp({ objects: [{ ...ready().objects[0] }, ...notSetUp().objects.slice(1)] }), "dev", IDLE)).toMatchObject({ status: "needs-attention" });
  });

  it("maps the samples' Workload states", () => {
    expect(sampleStatus({ kind: "raycluster", name: "x", present: true, state: "Running" })).toBe("ready");
    expect(sampleStatus({ kind: "raycluster", name: "x", present: true, state: "Inadmissible" })).toBe("needs-attention");
    expect(sampleStatus({ kind: "raycluster", name: "x", present: true, state: "Failed" })).toBe("error");
    expect(sampleStatus({ kind: "raycluster", name: "x", present: true })).toBe("setting-up");
    expect(sampleStatus({ kind: "rayjob", name: "x", present: false })).toBe("not-set-up");
  });
});

describe("Distributed workloads confirmations", () => {
  it("Set up: RayCluster on, RayJob off, the documented x86 image with why the CPU images are not used; other setups warn", () => {
    const setPending = vi.fn();
    const p = { kind: "setup-kueue" as const, project: "dev", rayCluster: true, rayJob: false, rayImage: KUEUE_DEFAULT_IMAGE };
    const spec = kueueConfirm(p, notSetUp(), setPending, ["a pipeline server", "a workbench"])!;
    expect(spec.title).toBe("Set up distributed workloads?");
    render(<>{spec.summary}{spec.choice}<ul>{spec.changes.map((c, i) => <li key={i}>{c}</li>)}</ul></>);
    expect(screen.getByRole("checkbox", { name: "RayCluster sample" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "RayJob sample" })).not.toBeChecked();
    expect(screen.getByRole("textbox", { name: "Ray image" })).toHaveValue("quay.io/modh/ray:2.47.1-py311-cu121");
    expect(screen.getByText(IMAGE_HELP)).toHaveTextContent("ppc64le");
    expect(screen.getByText(/already has other setups/)).toBeInTheDocument();
    expect(screen.getByText(/a pipeline server and a workbench/)).toBeInTheDocument();
    expect(screen.getByText(/for project dev only; not preemptive: a waiting workload never stops a running one/)).toBeInTheDocument();
    expect(screen.queryByText(/namespaceSelector|UIDs are recorded/)).toBeNull();
    expect(screen.getByText(/gets the label/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("checkbox", { name: "RayJob sample" }));
    expect(setPending).toHaveBeenCalledWith({ ...p, rayJob: true });
    // Invalid image input keeps Set up disabled.
    expect(kueueConfirm({ ...p, rayImage: "img; rm" }, notSetUp(), setPending)!.confirmDisabled).toBe(true);
    expect(kueueSteps(p).map((s) => s.id)).toEqual(["check", "queues", "project", "samples"]);
    expect(kueueSteps({ ...p, rayCluster: false }).map((s) => s.id)).toEqual(["check", "queues", "project"]);
  });

  it("the opt-in says it changes every new dashboard project; turning it off restores only what nobody changed", () => {
    const on = kueueConfirm({ kind: "kueue-optin" }, notSetUp({ objects: ready().objects }), () => {})!;
    render(<>{on.summary}<ul>{on.changes.map((c, i) => <li key={i}>{c}</li>)}</ul></>);
    expect(screen.getByText(OPT_IN_EFFECT)).toBeInTheDocument();
    expect(screen.getByText(/This changes every new project/)).toBeInTheDocument();
    expect(screen.getByText(/disableKueue: false/)).toBeInTheDocument();
    const off = kueueConfirm({ kind: "kueue-optout" }, ready(), () => {})!;
    render(<>{off.summary}<ul>{off.changes.map((c, i) => <li key={i}>{c}</li>)}</ul></>);
    expect(screen.getByText(/only if nobody changed it after this tool/)).toBeInTheDocument();
  });

  it("Clean up offers the setup, after the models and before the components, and the kueue line is explicit", () => {
    const c = cleanupCandidates(status, "kueue", false, null, null, null, null, ready());
    expect(c.kueue).toBe(true);
    expect(cleanupCandidates(status, "kueue", false, null, null, null, null, notSetUp()).kueue).toBe(false);
    expect(cleanupSteps({ kueue: true, models: ["dev"], components: ["kueue"] }).map((s) => s.id)).toEqual(["model:dev", "kueue", "components"]);
  });
});

describe("Distributed workloads tab", () => {
  const prereq = (ready: boolean) => ({ features: { kueue: { ready, parts: [{ id: "dsc-kueue", title: "Kueue (kueue)", state: ready ? "ready" : "missing", message: ready ? "Unmanaged in DataScienceCluster default-dsc" : "Removed in DataScienceCluster default-dsc. Set up sets it to Unmanaged; Clean up can turn it back." }], setupPlan: ready ? [] : ["DataScienceCluster default-dsc: set kueue to Unmanaged (it is Removed)"] } } });

  it("prerequisites missing: the parts and why Set up waits", async () => {
    stubApi({
      "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/kueue": notSetUp({ refusal: "Kueue's API (kueue.x-k8s.io) is not served yet", instance: { found: false, available: false, v1beta2: false, ready: false } }),
      "/api/resources/prerequisites": prereq(false),
    });
    renderAt("/test-resources?tab=kueue&project=dev");
    expect(await screen.findByText(/First set up what the distributed workloads setup needs/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Distributed workloads", level: 2 })).toBeInTheDocument();
    expect(screen.getByText(/Set up sets it to Unmanaged/)).toBeInTheDocument();
    expect(await screen.findByText(/First set up what the distributed workloads setup needs/)).toBeInTheDocument();
  });

  it("ready: the queues, the samples and the opt-in, with Clean up", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/kueue": ready(), "/api/resources/prerequisites": prereq(true) });
    renderAt("/test-resources?tab=kueue&project=dev");
    expect(await screen.findByRole("heading", { name: "RayCluster sample", level: 3 })).toBeInTheDocument();
    expect(screen.getByText(/added by this tool/)).toBeInTheDocument();
    expect(screen.getByText("Admitted")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Turn off..." })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Open Distributed workloads in the dashboard/ })).toHaveAttribute("href", "https://rh-ai.apps.example.com/observe-monitor/workload-metrics/workload-status/dev");
    fireEvent.click(screen.getByRole("button", { name: "Clean up distributed workloads..." }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("checkbox", { name: "Distributed workloads setup" })).toBeChecked();
    // RayJob sample can be enabled without turning on RayCluster.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    const add = await screen.findByRole("dialog");
    expect(within(add).getByRole("checkbox", { name: "RayJob sample" })).toBeChecked();
    expect(within(add).getByRole("checkbox", { name: "RayCluster sample" })).not.toBeChecked();
  });

  it("a setup in another project says so", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev", "other"] }, "/api/resources/kueue": { ...ready(), setUpIn: "dev" }, "/api/resources/prerequisites": prereq(true) });
    renderAt("/test-resources?tab=kueue&project=other");
    expect(await screen.findByText("Set up in project dev")).toBeInTheDocument();
  });
});

describe("review round: readiness and words", () => {
  it("M6: Ready only with Kueue running, no refusal, the label and active queues", () => {
    expect(kueueState({ ...ready(), instance: { ...ready().instance, ready: false } }, "dev", IDLE).status).not.toBe("ready");
    expect(kueueState({ ...ready(), instance: { ...ready().instance, v1beta2: false } }, "dev", IDLE).status).not.toBe("ready");
    expect(kueueState({ ...ready(), refusal: "RHOAI reports Kueue not ready" }, "dev", IDLE).status).not.toBe("ready");
    expect(kueueState({ ...ready(), label: { present: false } }, "dev", IDLE).status).not.toBe("ready");
    expect(kueueState({ ...ready(), objects: ready().objects.map((o) => ({ ...o, active: o.kind === "ResourceFlavor" })) }, "dev", IDLE).status).not.toBe("ready");
    expect(kueueState(ready(), "dev", IDLE).status).toBe("ready");
  });

  it("U2: the opt-in leads with its consequence, names the namespace and what Clean up leaves; one warning icon", () => {
    const spec = kueueConfirm({ kind: "kueue-optin" }, notSetUp({ objects: ready().objects }), () => {})!;
    expect(spec.title).toBe("Turn on the Dashboard Kueue setting?");
    expect(spec.titleIcon).toBeUndefined();
    render(<>{spec.summary}<ul>{spec.changes.map((c, i) => <li key={i}>{c}</li>)}</ul></>);
    expect(screen.getByText(/In every new project on this cluster, workbenches and model deployments can only use the queue hardware profile; a project without its own LocalQueue cannot create workbenches or deploy models until an admin adds one\./)).toBeInTheDocument();
    expect(screen.getByText(/in the applications namespace redhat-ods-applications/)).toBeInTheDocument();
    expect(screen.getByText(/If someone changes either after this, Clean up leaves it as it is/)).toBeInTheDocument();
    expect(screen.getByText(/Now: off \(the default\)\. This tool records it so Clean up can turn it back\./)).toBeInTheDocument();
    expect(spec.changes).toHaveLength(2);
    expect(screen.queryByText(/it is unset; the value before is recorded/)).toBeNull();
  });

  it("U3: Clean up names the profile, what happens to jobs people queued, and the component rule once", () => {
    render(<ul>{kueueCleanupLines(ready()).map((c, i) => <li key={i}>{c}</li>)}</ul>);
    expect(screen.getByText(/hardware profile nightly-kueue-cpu is deleted/)).toBeInTheDocument();
    expect(screen.getByText(/Jobs people sent to LocalQueue nightly-kueue themselves are never admitted once the queues are gone/)).toBeInTheDocument();
    expect(screen.queryByText(/DataScienceCluster/)).toBeNull();
  });

  it("U4/U6: the other-setups warning is a full sentence; the image help says why 2.47.1", () => {
    const p = { kind: "setup-kueue" as const, project: "dev", rayCluster: true, rayJob: false, rayImage: KUEUE_DEFAULT_IMAGE };
    const spec = kueueConfirm(p, notSetUp(), () => {}, ["a pipeline server", "a workbench"])!;
    render(<>{spec.choice}</>);
    expect(screen.getByText(/^This project already has a pipeline server and a workbench\. Kueue manages only/)).toBeInTheDocument();
    expect(IMAGE_HELP).toMatch(/newest x86 image the RHOAI docs list/);
    expect(IMAGE_HELP).toMatch(/do not run on x86 \(amd64\) clusters\./);
  });
});

describe("review round: Overview and prerequisites", () => {
  it("U1/U6: Clean up everything names the setup; the prerequisites button is short", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/kueue": ready(), "/api/resources/playground": { items: [] }, "/api/resources/workbench": { items: [] } });
    renderAt("/test-resources");
    expect(await screen.findByText(/the distributed workloads setup and the MaaS platform/)).toBeInTheDocument();
  });
});

describe("review round: the component rule once", () => {
  it("U3: the kueue component's condition is stated once in Clean up", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] }, "/api/resources/kueue": ready(),
      "/api/resources/prerequisites": { features: { kueue: { ready: true, parts: [], restorable: [{ component: "kueue", previous: "Removed", feature: "kueue" }] } } } });
    renderAt("/test-resources?tab=kueue&project=dev");
    fireEvent.click(await screen.findByRole("button", { name: "Clean up distributed workloads..." }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText(/no ClusterQueue or LocalQueue is left/)).toHaveLength(1);
    expect(within(dialog).queryByText(/still uses it/)).toBeNull();
  });
});

describe("final round: words", () => {
  it("one other setup: it keeps running; Clean up says if this tool still owns it, and whose jobs", () => {
    const p = { kind: "setup-kueue" as const, project: "dev", rayCluster: true, rayJob: false, rayImage: KUEUE_DEFAULT_IMAGE };
    render(<>{kueueConfirm(p, notSetUp(), () => {}, ["a pipeline server"])!.choice}</>);
    expect(screen.getByText(/This project already has a pipeline server\. Kueue manages only what is sent to a queue, so it keeps running\./)).toBeInTheDocument();
    render(<ul>{kueueCleanupLines(ready()).map((c, i) => <li key={i}>{c}</li>)}</ul>);
    expect(screen.getByText(/each only if this tool still owns it and nothing else uses it/)).toBeInTheDocument();
    expect(screen.getByText(/\(those jobs are deleted only when their project is deleted\)/)).toBeInTheDocument();
  });

  it("the prerequisites dialog is titled like its button and says each Clean up rule once", () => {
    const st = { features: { kueue: { ready: false, parts: [], setupPlan: [
      "Install Red Hat build of Kueue 1.5.1: Subscription openshift-kueue-operator/kueue-operator (package kueue-operator, channel stable-v1.5, from redhat-operators, automatic updates); it stays after Clean up, as other features share it",
      "DataScienceCluster default-dsc: set kueue to Unmanaged (it is Removed; Clean up can turn it back while nothing else uses it)",
    ] } } };
    const spec = prerequisitesConfirm({ kind: "setup-prerequisites", feature: "kueue" }, st)!;
    expect(spec.title).toBe("Set up Kueue's prerequisites?");
    render(<>{spec.summary}<ul>{spec.changes.map((c, i) => <li key={i}>{c}</li>)}</ul></>);
    expect(screen.getByText(/^Set up makes only what is missing/)).toBeInTheDocument();
    expect(screen.getAllByText(/stays? after Clean up/)).toHaveLength(1);
    expect(screen.getAllByText(/Clean up can turn/)).toHaveLength(1);
    expect(screen.getByText(/set kueue to Unmanaged \(it is Removed\)\./)).toBeInTheDocument();
  });

  it("the Queues card: one refusal sentence and a clear subtitle", async () => {
    stubApi({ "/api/resources/status": status, "/api/resources/projects": { projects: ["dev"] },
      "/api/resources/kueue": notSetUp({ refusal: "Set up its prerequisites first (above): Kueue is turned off on this cluster.", refusalCode: "prerequisites" }),
      "/api/resources/prerequisites": { features: { kueue: { ready: false, parts: [{ id: "dsc-kueue", title: "Kueue component", state: "missing", message: "Removed" }] } } } });
    renderAt("/test-resources?tab=kueue&project=dev");
    expect(await screen.findAllByText("Set up its prerequisites first (above): Kueue is turned off on this cluster.")).toHaveLength(1);
    expect(screen.getByText("Queues for project dev (the ClusterQueue and ResourceFlavor are cluster-wide objects; only this project uses them)")).toBeInTheDocument();
  });
});
