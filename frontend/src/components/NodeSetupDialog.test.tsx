import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { NodeSetupDialog } from "./NodeSetupDialog";
import { ImageMirrorSetup } from "./ImageMirrorSetup";
import { nightlyStatus, renderWithApp, settleApps } from "../test/providers";
import type { NodeSetupPlan, StatusResponse } from "../types";

const READY_PLAN: NodeSetupPlan = {
  action: "pull-secret-add",
  controlPlane: "self-managed",
  ready: true,
  state: "openshift-config/pull-secret has no quay.io/rhoai entry.",
  changes: ["Secret openshift-config/pull-secret: add a quay.io/rhoai entry with the token saved under Pull secret; the other 2 registry logins stay as they are."],
  nodeImpact: "The Machine Config Operator writes the new cluster pull secret to every node, one node at a time in each machine config pool, without draining or rebooting them.",
  undo: "Remove from the nodes' pull secret (the same item offers it): it removes only the quay.io/rhoai entry.",
  resourceVersion: "42",
};

function sse(...steps: object[]): Response {
  const body = steps.map((s) => `data: ${JSON.stringify(s)}\n\n`).join("");
  return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
}

/** fetch with a plan answer and, for the stream, the given steps. */
function stubBackend(plan: NodeSetupPlan | { status: number; body: object }, steps: object[] = []) {
  const fetchMock = vi.fn(async (url: string, _init?: RequestInit) => {
    if (url === "/api/setup/nodes/plan") {
      if ("status" in plan) return new Response(JSON.stringify(plan.body), { status: plan.status, headers: { "Content-Type": "application/json" } });
      return new Response(JSON.stringify(plan), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    if (url === "/api/setup/nodes/stream") return sse(...steps);
    return new Response("{}", { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

describe("NodeSetupDialog", () => {
  it("shows the plan (what changes, what the nodes do, how to undo), then runs it with the plan's version", async () => {
    const fetchMock = stubBackend(READY_PLAN, [
      { step: "check", status: "success", message: READY_PLAN.state },
      { step: "dry_run", status: "success", message: "The API server accepts it" },
      { step: "apply", status: "success", message: "Added the quay.io/rhoai entry to openshift-config/pull-secret." },
      { step: "verify", status: "success", message: "The nodes take it one at a time." },
      { step: "operation_complete", status: "success", message: "Added the quay.io/rhoai entry to openshift-config/pull-secret. The nodes take it one at a time." },
    ]);
    const onChanged = vi.fn();
    renderWithApp(<NodeSetupDialog action="pull-secret-add" onClose={vi.fn()} onChanged={onChanged} />);
    expect(await screen.findByText(/add a quay.io\/rhoai entry with the token saved under Pull secret/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "What the nodes do" })).toBeInTheDocument();
    expect(screen.getByText(/without draining or rebooting them/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "How to undo it" })).toBeInTheDocument();
    const planCall = fetchMock.mock.calls.find(([u]) => u === "/api/setup/nodes/plan");
    expect(JSON.parse(String(planCall?.[1]?.body))).toEqual({ action: "pull-secret-add" });

    fireEvent.click(screen.getByRole("button", { name: "Add to the nodes' pull secret" }));
    expect(await screen.findByText("Done")).toBeInTheDocument();
    expect(screen.getAllByText(/Added the quay.io\/rhoai entry to openshift-config\/pull-secret/).length).toBeGreaterThan(0);
    const streamCall = fetchMock.mock.calls.find(([u]) => u === "/api/setup/nodes/stream");
    expect(JSON.parse(String(streamCall?.[1]?.body))).toEqual({ action: "pull-secret-add", resourceVersion: "42" });
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    await settleApps();
  });

  it("explains a refusal and offers no confirm button", async () => {
    stubBackend({ action: "mirror-create", ready: false, state: "", refusalCode: "forbidden", refusal: "The cluster refused it in a dry run: admission webhook \"x.managed.openshift.io\" denied the request. Nothing was changed. Use the instructions under Do it yourself instead." });
    renderWithApp(<NodeSetupDialog action="mirror-create" onClose={vi.fn()} onChanged={vi.fn()} />);
    expect(await screen.findByText("The cluster does not allow this change here")).toBeInTheDocument();
    expect(screen.getByText(/denied the request/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create the image mirror" })).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Close" }).some((b) => b.textContent === "Close")).toBe(true);
    await settleApps();
  });

  it.each([
    ["parent_scope", "Another mirror serves these images now"],
    ["policy_conflict", "Another mirror sets a different source policy"],
    ["unknown_platform", "Not changed: this may be a Red Hat-managed cluster"],
  ] as const)("titles the %s refusal", async (code, title) => {
    stubBackend({ action: "mirror-create", ready: false, state: "", refusalCode: code, refusal: "Why. Nothing was changed." });
    renderWithApp(<NodeSetupDialog action="mirror-create" onClose={vi.fn()} onChanged={vi.fn()} />);
    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create the image mirror" })).not.toBeInTheDocument();
    await settleApps();
  });

  it("says when there is nothing to change", async () => {
    stubBackend({ action: "mirror-create", ready: false, nothingToDo: true, state: "ImageDigestMirrorSet team-mirrors already redirects registry.redhat.io/rhoai, so the mirror is set up." });
    renderWithApp(<NodeSetupDialog action="mirror-create" onClose={vi.fn()} onChanged={vi.fn()} />);
    expect(await screen.findByText("Nothing to change")).toBeInTheDocument();
    expect(screen.getByText(/team-mirrors already redirects/)).toBeInTheDocument();
    await settleApps();
  });

  it("leaves an entry this tool did not add: offers the check, and replaces it only after the acknowledgement", async () => {
    const refused: NodeSetupPlan = {
      action: "pull-secret-add", controlPlane: "self-managed", ready: false, state: "", refusalCode: "foreign_entry", replaceable: true,
      entrySetBy: "other", existingKeys: ["quay.io/rhoai"], resourceVersion: "42",
      refusal: "openshift-config/pull-secret already has a quay.io/rhoai entry (quay.io/rhoai) that this tool did not add (this tool has no record of adding it), with another token. It is not replaced.",
    };
    const replacing: NodeSetupPlan = {
      ...READY_PLAN, entrySetBy: "other", replacing: true,
      changes: ["Secret openshift-config/pull-secret: replace its quay.io/rhoai entry (quay.io/rhoai), which this tool did not add, with one that has the token saved under Pull secret; the other 2 registry logins stay as they are."],
      undo: "The token it has now is not kept, and this tool cannot restore it.",
    };
    const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/setup/nodes/plan") {
        const body = JSON.parse(String(init?.body));
        return new Response(JSON.stringify(body.replaceExisting ? replacing : refused), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      if (url === "/api/setup/nodes/stream") return sse({ step: "operation_complete", status: "success", message: "Replaced." });
      return new Response("{}", { status: 404 });
    });
    vi.stubGlobal("fetch", fetchMock);
    const onCheck = vi.fn();
    renderWithApp(<NodeSetupDialog action="pull-secret-add" onClose={vi.fn()} onChanged={vi.fn()} onCheck={onCheck} />);
    expect(await screen.findByText("An entry this tool did not add is already there")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add to the nodes' pull secret" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Check that the nodes can pull" }));
    expect(onCheck).toHaveBeenCalled();

    const ack = screen.getByRole("checkbox", { name: "Replace it with the token saved under Pull secret" });
    expect(ack).not.toBeChecked();
    expect(screen.getByText(/is not kept, and this tool cannot restore it/)).toBeInTheDocument();
    fireEvent.click(ack);
    const confirm = await screen.findByRole("button", { name: "Replace the entry" });
    expect(confirm).toHaveClass("pf-m-danger");
    const planCalls = fetchMock.mock.calls.filter(([u]) => u === "/api/setup/nodes/plan").map(([, i]) => JSON.parse(String(i?.body)));
    expect(planCalls).toEqual([{ action: "pull-secret-add" }, { action: "pull-secret-add", replaceExisting: true }]);
    fireEvent.click(confirm);
    expect(await screen.findByText("Done")).toBeInTheDocument();
    const streamCall = fetchMock.mock.calls.find(([u]) => u === "/api/setup/nodes/stream");
    expect(JSON.parse(String(streamCall?.[1]?.body))).toEqual({ action: "pull-secret-add", resourceVersion: "42", replaceExisting: true });
    await settleApps();
  });

  it("refuses to remove an entry this tool did not add", async () => {
    stubBackend({ action: "pull-secret-remove", ready: false, state: "", refusalCode: "not_owned", entrySetBy: "other",
      refusal: "The quay.io/rhoai entry in openshift-config/pull-secret was not added by this tool (or was changed since), so it is not removed here (this tool has no record of adding it). Nothing was changed." });
    renderWithApp(<NodeSetupDialog action="pull-secret-remove" onClose={vi.fn()} onChanged={vi.fn()} />);
    expect(await screen.findByText("Not added by this tool")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove from the nodes' pull secret" })).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    await settleApps();
  });

  it("shows a failed run's message", async () => {
    stubBackend({ ...READY_PLAN, action: "pull-secret-remove" }, [
      { step: "check", status: "failed", errorCode: "conflict", message: "It changed since you reviewed it, so nothing was changed. Review it again." },
      { step: "operation_complete", status: "failed", errorCode: "conflict", message: "It changed since you reviewed it, so nothing was changed. Review it again." },
    ]);
    renderWithApp(<NodeSetupDialog action="pull-secret-remove" onClose={vi.fn()} onChanged={vi.fn()} />);
    const confirm = await screen.findByRole("button", { name: "Remove from the nodes' pull secret" });
    expect(confirm).toHaveClass("pf-m-danger");
    fireEvent.click(confirm);
    expect(await screen.findByText("It stopped")).toBeInTheDocument();
    expect(screen.getAllByText(/It changed since you reviewed it/).length).toBeGreaterThan(0);
    await settleApps();
  });
});

function mirrorStatus(controlPlane: StatusResponse["cluster"]["controlPlane"], mirror: StatusResponse["imageMirror"]): StatusResponse {
  return nightlyStatus({ cluster: { server: "s", version: "4.22.8", user: "me", controlPlane }, imageMirror: mirror });
}
const MISSING = { exists: false, name: "", source: "" };
const OURS = { exists: true, name: "rhoai-quay-mirror", source: "registry.redhat.io/rhoai" };

describe("The image mirror row, by control plane", () => {
  it("self-managed: Create the image mirror, with the oc commands folded away", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByRole("button", { name: "Create the image mirror" })).toHaveClass("pf-m-primary");
    expect(screen.queryByRole("button", { name: "How to create the image mirror" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Do it yourself instead" }));
    expect(document.getElementById("image-mirror-create-command-code")).toHaveTextContent(/oc create -f -/);
    expect(document.getElementById("image-mirror-remove-command-code")).toHaveTextContent("oc delete imagedigestmirrorset rhoai-quay-mirror");
  });

  it("self-managed with the setup's mirror: Remove the image mirror", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", OURS)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove the image mirror" })).toHaveClass("pf-m-danger");
  });

  it("another mirror for the source counts as done and is not offered for removal", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", { exists: true, name: "team-mirrors", source: "registry.redhat.io/rhoai" })} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove the image mirror" })).not.toBeInTheDocument();
  });

  it("an ImageContentSourcePolicy of the setup's name is someone else's: named with its kind, not offered for removal", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", { ...OURS, kind: "ImageContentSourcePolicy" })} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText(/rhoai-quay-mirror \(ImageContentSourcePolicy\)/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove the image mirror" })).not.toBeInTheDocument();
  });

  it("managed classic: Create the image mirror too (its webhook allows registry.redhat.io/rhoai)", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("managed", MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByRole("button", { name: "Create the image mirror" })).toBeInTheDocument();
  });

  it("hosted: the rosa instructions, and why", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("hosted", MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByRole("button", { name: "How to create the image mirror" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create the image mirror" })).not.toBeInTheDocument();
    expect(screen.getByText(/configured through OpenShift Cluster Manager \(rosa create image-mirror\)/)).toBeInTheDocument();
  });

  it("unknown: the instructions first, and an attempt", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus(undefined, MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.getByRole("button", { name: "How to create the image mirror" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try to create it here" })).toHaveClass("pf-m-secondary");
  });

  it("the gitops profile only shows the state", async () => {
    await renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", MISSING)} readOnly onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />).settled();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("is disabled for a read-only user", async () => {
    renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />, { permissions: async () => ({ canMutate: false, user: "viewer" }) });
    await waitFor(() => expect(screen.getByRole("button", { name: "Create the image mirror" })).toHaveAttribute("aria-disabled", "true"));
    await settleApps();
  });

  it("opens the review dialog", async () => {
    stubBackend({ ...READY_PLAN, action: "mirror-create", changes: ["Create ImageDigestMirrorSet rhoai-quay-mirror (cluster-wide)."] });
    renderWithApp(<ImageMirrorSetup status={mirrorStatus("self-managed", MISSING)} readOnly={false} onShowInstructions={vi.fn()} onStatusRefresh={vi.fn()} />);
    const create = screen.getByRole("button", { name: "Create the image mirror" });
    await waitFor(() => expect(create).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(create);
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText("Create ImageDigestMirrorSet rhoai-quay-mirror (cluster-wide).")).toBeInTheDocument();
    await settleApps();
  });
});
