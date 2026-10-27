import { describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { catalogImageSetter, catalogManaged, gitOpsCreateDSCReason, gitOpsDSCReason, gitOpsFixReason, gitOpsInstallState, gitOpsInstallWaitReason, gitOpsOperatorReason, operatorGitOpsApp, operatorManaged } from "./gitops";
import { GitOpsNotice } from "./components/GitOpsNotice";
import { InstalledBuildCard } from "./components/StatusCards";
import { UpdateConfirmModal, UpdatePanel } from "./components/UpdatePanel";
import { pipelineStepsFor } from "./components/UpdatePipeline";
import { STEP_SETS } from "./operationSteps";
import { StatusPage } from "./pages/StatusPage";
import { ComponentsPage, IMAGE_LABELS_OFF } from "./pages/ComponentsPage";
import { HelpButton } from "./components/HelpModal";
import { nightlyStatus, renderPage, renderWithApp } from "./test/providers";
import { stubApi } from "./test/apiStub";

const GIT_IMAGE = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + "1".repeat(64);
const base = nightlyStatus();
const managed = nightlyStatus({
  subscription: { name: "rhods-operator", source: "rhoai-catalog-dev", channel: "beta", state: "AtLatestKnown", managedBy: "rhai-on-openshift" },
  csv: { name: "rhods-operator.3.6.0-ea.1", version: "3.6.0-ea.1", phase: "Succeeded" },
  catalogSource: { exists: true, name: "rhoai-catalog-dev", image: GIT_IMAGE, state: "READY", managedBy: "rhai-on-openshift" },
  nightly: { ...base.nightly, installed: { ...base.nightly!.installed!, image: GIT_IMAGE, digest: "sha256:" + "1".repeat(64) } },
  catalogServes: { channel: "beta", csv: "rhods-operator.3.6.0-ea.1", built: "2026-10-08T04:00:00Z", installed: false },
  dscManagedBy: "rhai-on-openshift",
});
/** The operator runs what the catalog serves; Quay has a newer build Git does not use. */
const synced = nightlyStatus({ ...managed, catalogServes: { ...managed.catalogServes!, installed: true } });

describe("GitOps helpers", () => {
  it("name the Argo CD application, or nothing without one", () => {
    expect(operatorGitOpsApp(managed)).toBe("rhai-on-openshift");
    expect(operatorGitOpsApp(nightlyStatus())).toBeUndefined();
    expect(gitOpsOperatorReason(nightlyStatus(), "Reinstall")).toBeNull();
    expect(gitOpsOperatorReason(managed, "Reinstall")).toMatch(/^Argo CD application "rhai-on-openshift" manages the operator\. Reinstall/);
    expect(gitOpsDSCReason(managed)).toMatch(/Change the DataScienceCluster in Git/);
    expect(gitOpsCreateDSCReason(managed)).toMatch(/creates the DataScienceCluster from Git/);
    expect(gitOpsCreateDSCReason(base)).toBeNull();
    expect(gitOpsInstallState(managed)).toBe("pending");
    expect(gitOpsInstallState(synced)).toBe("installed");
    expect(gitOpsInstallState(nightlyStatus({ ...managed, catalogServes: undefined }))).toBe("unknown");
    expect(gitOpsInstallState(nightlyStatus({ ...synced, csv: { name: "", version: "", phase: "Not Found" } }))).toBe("pending");
    expect(gitOpsInstallState(nightlyStatus({ ...synced, csv: { name: "", version: "", phase: "Not Found" }, installPlan: { name: "install-1", phase: "Installing", approved: true } }))).toBe("installing");
    expect(gitOpsInstallState(nightlyStatus({ ...synced, catalogServes: { ...synced.catalogServes!, installed: undefined, updating: true } }))).toBe("updating");
    expect(gitOpsInstallWaitReason("updating")).toMatch(/replacing the catalog pod/);
    expect(gitOpsInstallWaitReason("pending")).toBeNull();
    expect(gitOpsInstallState(nightlyStatus())).toBeNull();
  });

  it("turn off only the Diagnostics fixes that edit the DSC", () => {
    expect(gitOpsFixReason(managed, "disable-component:ray")).not.toBeNull();
    expect(gitOpsFixReason(managed, "delete-stale-webhooks")).toBeNull();
    expect(gitOpsFixReason(nightlyStatus(), "disable-component:ray")).toBeNull();
  });

  it("describe Update's GitOps steps with the same ids", () => {
    const plain = pipelineStepsFor("update", []);
    const gitOps = pipelineStepsFor("update", [], true);
    expect(gitOps.map((s) => s.id)).toEqual(plain.map((s) => s.id));
    expect(gitOps.find((s) => s.id === "apply_catalog_source")?.description).toMatch(/as Git sets it; the Subscription stays/);
    expect(pipelineStepsFor("refresh", [], true)).toBe(STEP_SETS.refresh);
  });
});

describe("GitOpsNotice", () => {
  it("lists what the tool still does for each managed part", () => {
    render(<GitOpsNotice status={managed} />);
    expect(screen.getByText("Argo CD manages the operator Subscription and nightly catalog, and the DataScienceCluster")).toBeInTheDocument();
    expect(screen.getByText(/Git picks the nightly build/)).toBeInTheDocument();
    expect(screen.getByText(/keeps the channel Git sets \(beta\)/)).toBeInTheDocument();
    expect(screen.queryByText(/ignoreDifferences/)).not.toBeInTheDocument();
    expect(screen.getByText(/Re-deploy and Reinstall are off/)).toBeInTheDocument();
    expect(screen.getByText(/Changes to the DataScienceCluster are off/)).toBeInTheDocument();
  });

  it("only mentions the DSC when only the DSC is managed", () => {
    render(<GitOpsNotice status={nightlyStatus({ dscManagedBy: "dsc-app" })} />);
    expect(screen.getByText("Argo CD manages the DataScienceCluster")).toBeInTheDocument();
    expect(screen.queryByText(/Re-deploy and Reinstall are off/)).not.toBeInTheDocument();
  });
});

describe("ComponentsPage under Argo CD", () => {
  it("turns off creating the DataScienceCluster by hand, which Git creates", async () => {
    stubApi({
      "/api/components": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded" },
    });
    renderPage(<ComponentsPage />, "/components", { status: async () => nightlyStatus({ ...synced, dscExists: false }) });
    expect(await screen.findByRole("heading", { name: "No DataScienceCluster yet" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Preview and create DataScienceCluster" })).toHaveAttribute("aria-disabled", "true"));
    fireEvent.mouseEnter(screen.getByRole("button", { name: "Preview and create DataScienceCluster" }));
    expect(await screen.findByText(/creates the DataScienceCluster from Git on its next sync/)).toBeInTheDocument();
  });
});

describe("StatusPage under Argo CD", () => {
  it("shows the notice and turns off Re-deploy and Reinstall with the reason", async () => {
    stubApi({ "/api/latest-nightly": { latest: managed.nightly?.latest }, "/api/nightly-tags": { tags: [] } });
    renderPage(<StatusPage />, "/", { status: async () => managed });
    expect(await screen.findByText(/Argo CD manages the operator Subscription/)).toBeInTheDocument();
    const recovery = screen.getByRole("list", { name: "Recovery actions" });
    await waitFor(() => expect(within(recovery).getByRole("button", { name: "Re-deploy operator..." })).toHaveAttribute("aria-disabled", "true"));
    expect(within(recovery).getByRole("button", { name: "Reinstall..." })).toHaveAttribute("aria-disabled", "true");
  });

  it("does not offer to create the DataScienceCluster, which Git creates", async () => {
    stubApi({ "/api/latest-nightly": { latest: managed.nightly?.latest }, "/api/nightly-tags": { tags: [] } });
    renderPage(<StatusPage />, "/", { status: async () => nightlyStatus({ ...synced, dscExists: false }) });
    expect(await screen.findByText("No DataScienceCluster yet")).toBeInTheDocument();
    expect(screen.queryByText("Create the DataScienceCluster")).not.toBeInTheDocument();
  });

  it("shows no notice without Argo CD", async () => {
    stubApi({ "/api/latest-nightly": { latest: managed.nightly?.latest }, "/api/nightly-tags": { tags: [] } });
    renderPage(<StatusPage />, "/", { status: async () => nightlyStatus() });
    const recovery = await screen.findByRole("list", { name: "Recovery actions" });
    await waitFor(() => expect(within(recovery).getByRole("button", { name: "Reinstall..." })).not.toHaveAttribute("aria-disabled"));
    expect(screen.queryByText(/Argo CD manages/)).not.toBeInTheDocument();
  });
});

function renderCard(status: typeof managed) {
  const onUpdate = vi.fn();
  renderWithApp(
    <InstalledBuildCard status={status} latest={null} latestLoading={false} blocker={null} prerequisitesMet onUpdate={onUpdate} onChooseBuild={vi.fn()} onPreview={vi.fn()} onRedeploy={vi.fn()} />,
  );
  return { onUpdate };
}

describe("InstalledBuildCard under Argo CD", () => {
  it("offers to install the build the catalog serves when the operator does not run it yet", () => {
    const { onUpdate } = renderCard(managed);
    expect(screen.getByText("Install pending")).toBeInTheDocument();
    const installed = screen.getByText("Installed").closest(".pf-v6-c-description-list__group") as HTMLElement;
    expect(within(installed).getByText("rhods-operator.3.6.0-ea.1")).toBeInTheDocument();
    expect(within(installed).getByText(/not the build the catalog serves/)).toBeInTheDocument();
    const inGit = screen.getByText("In Git").closest(".pf-v6-c-description-list__group") as HTMLElement;
    expect(within(inGit).getByText("111111111111")).toBeInTheDocument();
    expect(within(inGit).getByText(/set by Argo CD application "rhai-on-openshift"; serves rhods-operator.3.6.0-ea.1/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Install the build the catalog serves" }));
    expect(onUpdate).toHaveBeenCalledWith(expect.objectContaining({ image: GIT_IMAGE }));
    expect(screen.queryByRole("button", { name: "Choose another build" })).not.toBeInTheDocument();
    expect(screen.queryByText("Update available")).not.toBeInTheDocument();
  });

  it("waits while the catalog pod is replaced or OLM runs the first install", () => {
    renderCard(nightlyStatus({ ...synced, catalogServes: { ...synced.catalogServes!, installed: undefined, updating: true } }));
    expect(screen.getByText("Catalog updating")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Install the build the catalog serves" })).toHaveAttribute("aria-disabled", "true");
    cleanup();
    renderCard(nightlyStatus({ ...synced, csv: { name: "", version: "", phase: "Not Found" }, installPlan: { name: "install-1", phase: "Installing", approved: true } }));
    expect(screen.getByText("OLM installing")).toBeInTheDocument();
    expect(screen.getAllByText(/OLM is installing the build Git set/).length).toBeGreaterThanOrEqual(2); // the Installed row and the alert
    expect(screen.getByRole("button", { name: "Install the build the catalog serves" })).toHaveAttribute("aria-disabled", "true");
  });

  it("says the operator matches Git, and that a newer nightly is a change to make in Git", () => {
    renderCard(synced);
    expect(screen.getByText("Matches Git")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Install from the catalog again" })).toBeInTheDocument();
    expect(screen.getByText(/A newer rhoai-3.6 nightly exists; Git does not use it yet/)).toBeInTheDocument();
    expect(screen.getByText(synced.nightly!.latest!.image)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Update to latest/ })).not.toBeInTheDocument();
  });
});

describe("UpdatePanel under Argo CD", () => {
  it("fixes the image to Git's and installs what the catalog serves", async () => {
    const onRequestUpdate = vi.fn();
    renderWithApp(<UpdatePanel status={managed} latest={{ tag: "rhoai-3.6", image: base.nightly!.latest!.image }} onRequestUpdate={onRequestUpdate} />);
    const input = screen.getByLabelText("FBC image") as HTMLInputElement;
    expect(input.value).toBe(GIT_IMAGE);
    expect(input).toHaveAttribute("readonly");
    expect(screen.queryByRole("button", { name: "Fetch latest" })).not.toBeInTheDocument();
    expect(screen.getByText(/sets this image in Git/)).toBeInTheDocument();
    expect(screen.getByText(/The catalog serves rhods-operator.3.6.0-ea.1 \(built Oct 8, .*\), which is not installed/)).toBeInTheDocument();
    // The button is remounted once the permission check ends (TooltipButton drops its Tooltip), so query it again.
    await waitFor(() => expect(screen.getByRole("button", { name: "Install the build the catalog serves" })).not.toHaveAttribute("aria-disabled"));
    fireEvent.click(screen.getByRole("button", { name: "Install the build the catalog serves" }));
    expect(onRequestUpdate).toHaveBeenCalledWith(expect.objectContaining({ image: GIT_IMAGE }));
  });

  it("does not call the catalog's build installed while the catalog pod is replaced", () => {
    const updating = nightlyStatus({ ...synced, catalogServes: { ...synced.catalogServes!, updating: true } });
    renderWithApp(<UpdateConfirmModal target={updating.nightly!.installed!} status={updating} onConfirm={vi.fn()} onClose={vi.fn()} />);
    expect(screen.queryByText(/already installed/)).not.toBeInTheDocument();
    cleanup();
    const unknown = nightlyStatus({ ...managed, catalogServes: undefined });
    renderWithApp(<UpdateConfirmModal target={unknown.nightly!.installed!} status={unknown} onConfirm={vi.fn()} onClose={vi.fn()} />);
    expect(screen.queryByText(/already installed/)).not.toBeInTheDocument();
    cleanup();
    renderWithApp(<UpdateConfirmModal target={synced.nightly!.installed!} status={synced} onConfirm={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText(/already installed/)).toBeInTheDocument();
  });

  it("explains the GitOps install in the confirmation", () => {
    renderWithApp(<UpdateConfirmModal target={managed.nightly!.installed!} status={managed} onConfirm={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText("Install the build the catalog serves?")).toBeInTheDocument();
    const installed = screen.getByText("Installed").closest(".pf-v6-c-description-list__group") as HTMLElement;
    expect(within(installed).getByText(/rhods-operator.3.6.0-ea.1/)).toBeInTheDocument();
    expect(within(installed).getByText(/not the build the catalog serves/)).toBeInTheDocument();
    expect(screen.getByText(/The catalog image stays as Git sets it\. Update installs the build the catalog serves \(rhods-operator.3.6.0-ea.1\) and keeps the channel Git sets \(beta\)/)).toBeInTheDocument();
    expect(screen.queryByText(/already installed/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Install" })).toBeInTheDocument();
  });
});

// An ACM ConfigurationPolicy enforces the catalog (no Argo CD tracking on
// it); Argo CD manages the Subscription, or nothing does.
const acmCatalog = { ...managed.catalogSource, managedBy: undefined, managedByACM: true };
const acmWithArgoSub = nightlyStatus({ ...managed, catalogSource: acmCatalog, dscManagedBy: undefined });
const acmOnly = nightlyStatus({ ...acmWithArgoSub, subscription: { ...managed.subscription, managedBy: undefined } });
const gitOpsVersion = async () => ({ version: "dev", commit: "x", buildDate: "x", templateOutdated: false, profile: "gitops" as const, admissionPolicy: "enforced" });

describe("A catalog an ACM policy manages", () => {
  it("counts as managed and is named as the policy", () => {
    for (const s of [acmWithArgoSub, acmOnly]) {
      expect(operatorManaged(s)).toBe(true);
      expect(catalogManaged(s)).toBe(true);
      expect(catalogImageSetter(s)).toBe("an ACM policy (config-policy-controller)");
      expect(gitOpsInstallState(s)).toBe("pending");
    }
    expect(gitOpsOperatorReason(acmOnly, "Reinstall")).toMatch(/^An ACM policy \(config-policy-controller\) manages the nightly catalog .* Reinstall is off/);
    // Only Argo CD renders the DSC from Git.
    expect(gitOpsCreateDSCReason(acmOnly)).toBeNull();
    // Argo CD on the catalog wins over an ACM entry, and nothing is managed without either.
    expect(catalogImageSetter(nightlyStatus({ ...managed, catalogSource: { ...managed.catalogSource, managedByACM: true } }))).toBe('Argo CD application "rhai-on-openshift"');
    expect(operatorManaged(base)).toBe(false);
    // Argo CD on the Subscription alone does not let the gitops profile run.
    expect(catalogManaged(nightlyStatus({ ...managed, catalogSource: { ...managed.catalogSource, managedBy: undefined } }))).toBe(false);
  });

  it("says so in the notice", () => {
    render(<GitOpsNotice status={acmWithArgoSub} />);
    expect(screen.getByText("Argo CD manages the operator Subscription, and an ACM policy manages the nightly catalog")).toBeInTheDocument();
    expect(screen.getByText(/The ACM policy picks the nightly build/)).toBeInTheDocument();
    cleanup();
    render(<GitOpsNotice status={acmOnly} />);
    expect(screen.getByText("An ACM policy manages the nightly catalog")).toBeInTheDocument();
    expect(screen.getByText(/Re-deploy and Reinstall are off while the policy manages the catalog/)).toBeInTheDocument();
    expect(screen.queryByText(/Argo CD/)).not.toBeInTheDocument();
  });

  it("shows the policy as the source of the catalog image", () => {
    renderCard(acmWithArgoSub);
    expect(screen.getByText(/Catalog image set by an ACM policy \(config-policy-controller\); serves rhods-operator.3.6.0-ea.1/)).toBeInTheDocument();
    cleanup();
    renderCard(nightlyStatus({ ...acmOnly, catalogServes: { ...acmOnly.catalogServes!, installed: true } }));
    expect(screen.getByText(/An ACM policy \(config-policy-controller\) sets the catalog image\. To move to this build, change the image in the policy/)).toBeInTheDocument();
  });

  it("fixes the image to the policy's, also in the gitops profile", async () => {
    renderWithApp(<UpdatePanel status={acmOnly} latest={null} onRequestUpdate={vi.fn()} />, { version: gitOpsVersion });
    await waitFor(() => expect((screen.getByLabelText("FBC image") as HTMLInputElement).value).toBe(GIT_IMAGE));
    expect(screen.getByText("From the ACM policy")).toBeInTheDocument();
    expect(screen.getByText(/An ACM policy \(config-policy-controller\) sets this image/)).toBeInTheDocument();
    expect(screen.queryByText("Update is off for this catalog")).not.toBeInTheDocument();
  });
});

// The gitops profile never contacts Quay or any other registry: every line
// of build information comes from the cluster, and "Latest" says where a
// newer build comes from instead of looking it up.
describe("The gitops profile reads no registry", () => {
  const gitOpsStatus = nightlyStatus({ ...synced, nightly: { installed: synced.nightly!.installed }, pullSecret: { exists: false, valid: false } });

  function renderGitOpsCard(status: typeof managed) {
    renderWithApp(
      <InstalledBuildCard status={status} latest={null} latestLoading={false} blocker={null} prerequisitesMet onUpdate={vi.fn()} onChooseBuild={vi.fn()} onRedeploy={vi.fn()} />,
      { version: gitOpsVersion },
    );
  }

  it("says that Git or the ACM policy picks the build, without a Quay lookup or error", async () => {
    renderGitOpsCard(nightlyStatus({ ...gitOpsStatus, nightly: { ...gitOpsStatus.nightly, error: "Could not read the latest rhoai-3.6 build from Quay: manifest HEAD returned 401" } }));
    expect(await screen.findByText(/^Git picks the build\. A newer nightly arrives as a change in Git \(for example a Renovate or Dependabot pull request\)/)).toBeInTheDocument();
    expect(screen.queryByText(/401/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Could not read the latest/)).not.toBeInTheDocument();
    cleanup();
    renderGitOpsCard(nightlyStatus({ ...gitOpsStatus, catalogSource: acmCatalog }));
    expect(await screen.findByText(/^The ACM policy picks the build\. A newer nightly arrives as a change to the policy in Git/)).toBeInTheDocument();
    cleanup();
    // The full profile keeps its Quay lookup.
    renderCard(nightlyStatus({ ...gitOpsStatus, nightly: { ...gitOpsStatus.nightly, error: "Could not read the latest rhoai-3.6 build from Quay: x" } }));
    expect(screen.getByText(/Could not read the latest rhoai-3.6 build from Quay/)).toBeInTheDocument();
  });

  it("says in the help dialog that the updater's releases come through Git", async () => {
    renderWithApp(<HelpButton />, { version: gitOpsVersion, update: async () => ({ current: "v2.2.0", updateAvailable: false, majorUpgrade: false, pinnedInGit: true }) });
    await waitFor(() => expect(screen.getByRole("button", { name: /help/i })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /help/i }));
    expect(await screen.findByText(/The chart pins the updater's release in Git/)).toBeInTheDocument();
    cleanup();
    renderWithApp(<HelpButton />);
    fireEvent.click(screen.getByRole("button", { name: /help/i }));
    await screen.findByText("Updater build");
    expect(screen.queryByText(/The chart pins the updater's release in Git/)).not.toBeInTheDocument();
  });

  it("shows the operator build from the CSV, and why the images have no labels", async () => {
    stubApi({
      "/api/components": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded",
        imageLabelsOff: true, operatorImage: "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:abc", operatorBuilt: "2026-10-08T04:00:00Z" },
      "/api/components?labels=true": { components: [], deployments: [], dscName: "", dscPhase: "", changedCount: 0, dscExists: false, dscState: "no-dsc", operatorVersion: "3.6.0", operatorPhase: "Succeeded",
        imageLabelsOff: true, operatorImage: "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:abc", operatorBuilt: "2026-10-08T04:00:00Z" },
    });
    renderPage(<ComponentsPage />, "/components", { status: async () => gitOpsStatus, version: gitOpsVersion });
    expect(await screen.findByText(new RegExp(IMAGE_LABELS_OFF.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
    expect(screen.getByText("registry.redhat.io/rhoai/odh-rhel9-operator@sha256:abc")).toBeInTheDocument();
  });
});
