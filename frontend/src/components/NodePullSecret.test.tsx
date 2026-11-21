import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { PullSecretSetup } from "./PullSecretCard";
import { ClusterSetupCard } from "./StatusCards";
import { SetupModal } from "./PrerequisitesPanel";
import { nodePullUnverified, prerequisitesMet } from "../utils";
import { imageByTag } from "../services/api/operator";
import { nightlyStatus, renderWithApp, settleApps } from "../test/providers";
import { jsonBody } from "../test/utils";
import type { PullSecretInfo, StatusResponse } from "../types";

// The command the backend sends (pkg/cluster/internal/rhoai/node_pull_secret.sh); the card
// shows it as it comes.
const COMMAND = `printf 'quay.io/rhoai token (the one you saved under Pull secret; base64 user:password, with its = padding): ' && read -rs QUAY_AUTH && echo
[ -n "$QUAY_AUTH" ] && oc get secret pull-secret -n openshift-config -o json \\
  | QUAY_AUTH="$QUAY_AUTH" jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | .auths["quay.io/rhoai"] = {auth: $ENV.QUAY_AUTH} | tojson | @base64)' \\
  | oc replace -f -
unset QUAY_AUTH`;
const IMAGE = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "0b3f9b4d".padEnd(64, "1");

const REMOVE_COMMAND = `printf 'This removes the quay.io/rhoai entry of openshift-config/pull-secret, whoever added it: also one the cluster was set up with, whose token the updater cannot restore. Type yes to remove it: ' && read -r ANSWER && [ "$ANSWER" = yes ] && oc get secret pull-secret -n openshift-config -o json \\
  | jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | del(.auths["quay.io/rhoai"]) | tojson | @base64)' \\
  | oc replace -f -
unset ANSWER`;

const saved = (nodes?: PullSecretInfo["nodes"]): PullSecretInfo => ({ exists: true, valid: true, nodes });
const unchecked = { command: COMMAND, removeCommand: REMOVE_COMMAND, pulled: false };

/** A fresh cluster: the updater's copy saved, the mirror there, nothing installed. */
function freshStatus(controlPlane: StatusResponse["cluster"]["controlPlane"], pullSecret: PullSecretInfo): StatusResponse {
  return nightlyStatus({
    cluster: { server: "s", version: "4.22.8", user: "me", controlPlane },
    pullSecret,
    catalogSource: { exists: false, name: "rhoai-catalog-dev", image: "", state: "" },
  });
}

function stubCheck(answer: object) {
  const fetchMock = vi.fn(async (url: string, _init?: RequestInit) => {
    if (url === "/api/setup/node-pull-check") {
      return new Response(JSON.stringify(answer), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return new Response("{}", { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

describe("The pull secret's node part, by control plane", () => {
  it("is not there on a hosted control plane: the saved secret is Ready", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved()} controlPlane="hosted" />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.queryByText(/openshift-config\/pull-secret/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check that the nodes can pull" })).not.toBeInTheDocument();
  });

  it("on a self-managed cluster offers one click, keeps the commands folded away, and does not say Ready before the nodes are checked", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" checkImage={IMAGE} />).settled();
    expect(screen.getByText("Nodes not checked")).toBeInTheDocument();
    expect(screen.queryByText("Ready")).not.toBeInTheDocument();
    expect(screen.getByText(/This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config\/pull-secret/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add to the nodes' pull secret" })).toHaveClass("pf-m-primary");
    // No record: the removal is not offered, and who set an entry is not known.
    expect(screen.queryByRole("button", { name: "Remove from the nodes' pull secret" })).not.toBeInTheDocument();
    expect(screen.getByText(/^Not known \(not this tool\)/)).toBeInTheDocument();
    expect(document.getElementById("node-pull-secret-command-code")).not.toBeVisible();
    const toggle = screen.getByRole("button", { name: "Do it yourself instead" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(document.getElementById("node-pull-secret-command-code")?.textContent).toBe(COMMAND);
    expect(document.getElementById("node-pull-secret-remove-command-code")?.textContent).toBe(REMOVE_COMMAND);
    expect(screen.getByRole("button", { name: "Copy the command" })).toBeInTheDocument();
    expect(screen.getByText(/oc get mcp/)).toBeInTheDocument();
  });

  it("on a Red Hat-managed cluster offers no button and says why, with the commands open", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="managed" checkImage={IMAGE} />).settled();
    expect(screen.getByText(/Red Hat runs this cluster \(ROSA Classic or OpenShift Dedicated\)/)).toBeInTheDocument();
    expect(screen.getByText(/has no procedure for changing it, so the updater does not change it/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add to the nodes' pull secret" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "The commands" })).toHaveAttribute("aria-expanded", "true");
    expect(document.getElementById("node-pull-secret-command-code")?.textContent).toBe(COMMAND);
    expect(screen.getByRole("button", { name: "Check that the nodes can pull" })).toBeInTheDocument();
  });

  it("when the control plane is unknown shows the command first and offers the click as an attempt", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} checkImage={IMAGE} />).settled();
    expect(screen.getByText("Nodes not checked")).toBeInTheDocument();
    expect(screen.getByText(/could not read what kind of cluster this is/)).toBeInTheDocument();
    expect(screen.getByText(/If this cluster is self-managed, its nodes pull with the cluster pull secret/)).toBeInTheDocument();
    expect(screen.queryByText(/This cluster is self-managed:/)).not.toBeInTheDocument();
    expect(document.getElementById("node-pull-secret-command-code")?.textContent).toBe(COMMAND);
    expect(screen.getByRole("button", { name: "Add to the nodes' pull secret" })).toHaveClass("pf-m-secondary");
    expect(screen.queryByRole("button", { name: "Remove from the nodes' pull secret" })).not.toBeInTheDocument();
  });

  it("is not there before the updater's copy is saved; the form says it comes next", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={{ exists: false, valid: false, nodes: unchecked }} controlPlane="self-managed" />).settled();
    expect(screen.getByText("Missing")).toBeInTheDocument();
    expect(screen.getByText(/The nodes of this self-managed cluster need it in openshift-config\/pull-secret too: once it is saved, this item offers to add it there/)).toBeInTheDocument();
    expect(document.getElementById("node-pull-secret-command-code")).toBeNull();
    expect(screen.queryByRole("button", { name: "Check that the nodes can pull" })).not.toBeInTheDocument();
    cleanup();
    await renderWithApp(<PullSecretSetup pullSecret={{ exists: false, valid: false, nodes: unchecked }} />).settled();
    expect(screen.getByText(/If this cluster is self-managed, its nodes need it in openshift-config\/pull-secret too/)).toBeInTheDocument();
    cleanup();
    await renderWithApp(<PullSecretSetup pullSecret={{ exists: false, valid: false }} controlPlane="hosted" />).settled();
    expect(screen.queryByText(/openshift-config\/pull-secret/)).not.toBeInTheDocument();
  });

  it("is Ready, with the command folded away, when the nightly catalog shows that the nodes pull", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved({ ...unchecked, pulled: true })} controlPlane="self-managed" />).settled();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText(/The nightly catalog runs its quay.io\/rhoai image, so they can pull/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check that the nodes can pull" })).not.toBeInTheDocument();
    // The nodes pull and this tool has no record: someone else set it up.
    const already = screen.getByRole("button", { name: "Already set up (not by this tool)" });
    expect(already).toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByRole("button", { name: "Add to the nodes' pull secret" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove from the nodes' pull secret" })).not.toBeInTheDocument();
    expect(screen.getByText("Someone else: the nodes can pull, and this tool did not add an entry")).toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Do it yourself instead" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(document.getElementById("node-pull-secret-command-code")?.textContent).toBe(COMMAND);
    expect(document.getElementById("node-pull-secret-remove-command-code")?.textContent).toBe(REMOVE_COMMAND);
    expect(screen.getByText(/Remove the quay.io\/rhoai entry, whoever added it \(it asks first\)/)).toBeInTheDocument();
  });

  it("offers the removal only for the entry this tool recorded adding, and says who added it", async () => {
    const own = { ...unchecked, pulled: true, addedByTool: { at: "2026-11-18T10:00:00Z", by: "admin", sameToken: true } };
    await renderWithApp(<PullSecretSetup pullSecret={saved(own)} controlPlane="self-managed" />).settled();
    expect(screen.getByText(/^This tool \(by admin, /)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add to the nodes' pull secret" })).toHaveClass("pf-m-secondary");
    expect(screen.getByRole("button", { name: "Remove from the nodes' pull secret" })).toHaveClass("pf-m-danger");
  });

  it("offers to update the tool's own entry when the saved token changed since", async () => {
    const own = { ...unchecked, pulled: true, addedByTool: { at: "2026-11-18T10:00:00Z", by: "admin", sameToken: false } };
    await renderWithApp(<PullSecretSetup pullSecret={saved(own)} controlPlane="self-managed" />).settled();
    expect(screen.getByRole("button", { name: "Update the nodes' pull secret" })).toHaveClass("pf-m-primary");
    expect(screen.getByText(/with an earlier token than the one saved above/)).toBeInTheDocument();
  });

  it("is not shown in the gitops profile", async () => {
    await renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" readOnly />).settled();
    expect(screen.getByText("Not used")).toBeInTheDocument();
    expect(screen.queryByText(/openshift-config\/pull-secret/)).not.toBeInTheDocument();
  });
});

describe("imageByTag", () => {
  it("drops the digest and needs a tag", () => {
    expect(imageByTag(IMAGE)).toBe("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6");
    expect(imageByTag("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6")).toBe("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6");
    expect(imageByTag("quay.io/rhoai/rhoai-fbc-fragment@sha256:" + "a".repeat(64))).toBeNull();
    expect(imageByTag("registry.example.com:5000/rhoai/fbc")).toBeNull();
  });
});

describe("Check that the nodes can pull", () => {
  it("runs the check with the build Update would install and shows success", async () => {
    const fetchMock = stubCheck({ success: true, message: `A temporary catalog pod pulled ${IMAGE} with the nodes' pull secret, and was removed again.`, logs: [] });
    renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" checkImage={IMAGE} />);
    const check = screen.getByRole("button", { name: "Check that the nodes can pull" });
    await settleApps();
    await waitFor(() => expect(check).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(check);
    expect(await screen.findByText("The nodes can pull from quay.io/rhoai")).toBeInTheDocument();
    const call = fetchMock.mock.calls.find(([url]) => url === "/api/setup/node-pull-check");
    expect(call?.[1]?.method).toBe("POST");
    // By tag, without the digest: a cached digest is not pulled again.
    expect(jsonBody(call?.[1])).toEqual({ image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6" });
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.queryByText("Nodes not checked")).not.toBeInTheDocument();
    await settleApps();
  });

  it("shows why the nodes cannot pull", async () => {
    stubCheck({ success: false, errorCode: "catalog_image_pull", message: "The catalog image x cannot be pulled (ErrImagePull: unauthorized).", logs: [] });
    renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" checkImage={IMAGE} />);
    const check = screen.getByRole("button", { name: "Check that the nodes can pull" });
    await settleApps();
    await waitFor(() => expect(check).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(check);
    expect(await screen.findByText("The nodes cannot pull from quay.io/rhoai")).toBeInTheDocument();
    expect(screen.getByText(/ErrImagePull: unauthorized/)).toBeInTheDocument();
    expect(screen.getByText("Nodes can't pull")).toBeInTheDocument();
    await settleApps();
  });

  it("waits for the build to check, and says why", async () => {
    // Until the permission check answers, the tooltip says that instead: hover the settled page.
    await renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" checkImageReason="The newest nightly build could not be looked up: quay down" />).settled();
    const check = screen.getByRole("button", { name: "Check that the nodes can pull" });
    expect(check).toHaveAttribute("aria-disabled", "true");
    fireEvent.mouseEnter(check);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/quay down/);
    await settleApps();
  });

  it("is disabled for a read-only user", async () => {
    renderWithApp(<PullSecretSetup pullSecret={saved(unchecked)} controlPlane="self-managed" checkImage={IMAGE} />, { permissions: async () => ({ canMutate: false, user: "viewer" }) });
    await settleApps();
    await waitFor(() => expect(screen.getByRole("button", { name: "Check that the nodes can pull" })).toHaveAttribute("aria-disabled", "true"));
    await settleApps();
  });
});

describe("The cluster setup on a self-managed cluster", () => {
  it("does not count the pull secret as done before the nodes are checked, and does once they pass", async () => {
    stubCheck({ success: true, message: "ok", logs: [] });
    const status = freshStatus("self-managed", saved(unchecked));
    renderWithApp(<ClusterSetupCard status={status} onStatusRefresh={vi.fn()} onShowInstructions={vi.fn()} checkImage={IMAGE} />);
    expect(screen.getByText("1 of 2 ready")).toBeInTheDocument();
    const check = screen.getByRole("button", { name: "Check that the nodes can pull" });
    await settleApps();
    await waitFor(() => expect(check).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(check);
    await screen.findByText("The nodes can pull from quay.io/rhoai");
    const header = screen.getByRole("heading", { name: "One-time cluster setup" }).closest(".pf-v6-c-card__header") as HTMLElement;
    expect(within(header).getByText("Ready")).toBeInTheDocument();
    await settleApps();
  });

  it("keeps the card at the top (nodes unverified) but does not hold back Update", () => {
    const status = freshStatus("self-managed", saved(unchecked));
    expect(nodePullUnverified(status)).toBe(true);
    expect(prerequisitesMet(status)).toBe(true);
    expect(nodePullUnverified(freshStatus("self-managed", saved({ ...unchecked, pulled: true })))).toBe(false);
    expect(nodePullUnverified(freshStatus("hosted", saved()))).toBe(false);
    expect(nodePullUnverified(status, true)).toBe(false);
    expect(nodePullUnverified(freshStatus("self-managed", { exists: false, valid: false, nodes: unchecked }))).toBe(false);
  });
});

describe("The setup instructions by control plane", () => {
  it("show the rosa steps on ROSA HCP", async () => {
    await renderWithApp(<SetupModal isOpen onClose={vi.fn()} status={freshStatus("hosted", saved())} />).settled();
    expect(document.getElementById("cmd-image-mirror")).toHaveTextContent(/rosa create image-mirror/);
    expect(document.getElementById("cmd-image-mirror-oc")).toBeNull();
  });

  it("show the oc ImageDigestMirrorSet on a self-managed cluster, and point to the nodes' command", async () => {
    await renderWithApp(<SetupModal isOpen onClose={vi.fn()} status={freshStatus("self-managed", saved(unchecked))} />).settled();
    expect(document.getElementById("cmd-image-mirror-oc")).toHaveTextContent(/kind: ImageDigestMirrorSet/);
    expect(document.getElementById("cmd-image-mirror-oc")).toHaveTextContent(/name: rhoai-quay-mirror/);
    expect(document.getElementById("cmd-image-mirror")).toBeNull();
    expect(screen.getByText(/The nodes of a self-managed cluster pull with openshift-config\/pull-secret/)).toBeInTheDocument();
  });

  it("show both, labelled, when the control plane is unknown", async () => {
    await renderWithApp(<SetupModal isOpen onClose={vi.fn()} status={freshStatus(undefined, saved(unchecked))} />).settled();
    expect(screen.getByText("On ROSA HCP")).toBeInTheDocument();
    expect(screen.getByText("On self-managed OpenShift")).toBeInTheDocument();
    expect(document.getElementById("cmd-image-mirror")).not.toBeNull();
    expect(document.getElementById("cmd-image-mirror-oc")).not.toBeNull();
  });
});
