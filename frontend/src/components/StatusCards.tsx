import React, { useState } from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardExpandableContent,
  CardFooter,
  CardHeader,
  CardTitle,
  ClipboardCopy,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import ArrowCircleUpIcon from "@patternfly/react-icons/dist/esm/icons/arrow-circle-up-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import { useNavigate } from "react-router-dom";
import type { LatestNightlyResponse, NightlyBuild, StatusResponse } from "../types";
import { isOnNightly, tagLine } from "../build";
import { operatorInstalled, prerequisitesMet as setupComplete } from "../utils";
import { BuildSummary } from "./BuildSummary";
import { PullSecretSetup } from "./PullSecretCard";
import { CardList, CardListItem } from "./CardList";
import { StatusLabel, TagLabel } from "./StatusLabel";
import { TooltipButton } from "./TooltipButton";
import { catalogImageSetter, catalogManagedByACM, gitOpsInstallState, gitOpsInstallWaitReason, operatorManaged } from "../gitops";
import { useGitOpsProfile } from "../state/AppInfo";

/** CSV phases of an install OLM is still working on. */
const INSTALLING_PHASES = new Set(["Installing", "Replacing", "Pending", "InstallReady"]);

/** The CSV phase as a status label. */
export function PhaseLabel({ phase }: { phase: string }): React.ReactElement {
  switch (phase) {
    case "Succeeded":
      return <StatusLabel status="success">Succeeded</StatusLabel>;
    case "Failed":
      return <StatusLabel status="danger">Failed</StatusLabel>;
    case "Not Found":
      return <StatusLabel status="neutral">Not installed</StatusLabel>;
    default:
      if (INSTALLING_PHASES.has(phase)) {
        return <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>{phase}</StatusLabel>;
      }
      return <StatusLabel status="warning">{phase || "Unknown"}</StatusLabel>;
  }
}

interface InstalledBuildCardProps {
  status: StatusResponse;
  /** Newest nightly overall (GET /api/latest-nightly); used when status.nightly has no latest. */
  latest: LatestNightlyResponse | null;
  latestLoading: boolean;
  /** Why mutations are blocked now (permissions, another operation), or null. */
  blocker: string | null;
  prerequisitesMet: boolean;
  onUpdate: (target: NightlyBuild) => void;
  onChooseBuild: () => void;
  /** Absent where Build Explorer is off (the gitops profile). */
  onPreview?: (image: string) => void;
  onRedeploy: () => void;
}

/**
 * A07-1: what is installed, whether a newer nightly exists, and the primary
 * action, above the fold. Compares by digest (status.nightly, computed by the
 * backend against the newest build of the installed tag).
 */
export const InstalledBuildCard: React.FC<InstalledBuildCardProps> = ({
  status,
  latest,
  latestLoading,
  blocker,
  prerequisitesMet,
  onUpdate,
  onChooseBuild,
  onPreview,
  onRedeploy,
}) => {
  const navigate = useNavigate();
  const gitOpsProfile = useGitOpsProfile();
  const installed = operatorInstalled(status);
  const onNightly = isOnNightly(status);
  const nightly = status.nightly;
  const csv = status.csv;
  const safeConsoleURL = status.consoleURL?.startsWith("https://") ? status.consoleURL : "";

  // The newest build to offer: the same stream when on nightly, else the newest nightly overall.
  const latestOverall: NightlyBuild | null = latest?.image && !latest.error
    ? { image: latest.image, tag: latest.tag, digest: latest.digest, buildDate: latest.buildDate }
    : null;
  const target: NightlyBuild | null = nightly?.latest ?? latestOverall;
  const updateAvailable = nightly?.updateAvailable;
  // Under Argo CD, Git sets the catalog image: the only build to install is
  // the one the catalog serves, and a newer nightly in Quay is a change to
  // make in Git.
  const gitOpsApp = operatorManaged(status);
  const gitOpsState = gitOpsInstallState(status);
  const catalogBuild: NightlyBuild | null = gitOpsApp ? nightly?.installed ?? null : null;
  const serves = status.catalogServes;
  const previewBuild = catalogBuild ?? target;
  const csvMissing = !csv.name || csv.phase === "Not Found";
  const setupReason = !prerequisitesMet ? "Finish the one-time cluster setup first (pull secret and image mirror)." : null;
  const actionReason = blocker ?? setupReason;
  // An operator version is installed but its Subscription is gone (for
  // example after an interrupted Update or Reinstall): neither nightly nor
  // stable, and OLM no longer updates or repairs it.
  const subscriptionMissing = installed && !status.subscription.source;
  const csvInstalling = INSTALLING_PHASES.has(csv.phase);

  // The verdict never contradicts the operator row: a Failed or not yet
  // Succeeded CSV is not "Up to date".
  let verdict: React.ReactNode;
  if (!installed) verdict = <StatusLabel status="neutral">Not installed</StatusLabel>;
  else if (csv.phase === "Failed") verdict = <StatusLabel status="danger">Operator failed</StatusLabel>;
  else if (subscriptionMissing) verdict = <StatusLabel status="warning">No Subscription</StatusLabel>;
  else if (!onNightly) verdict = <TagLabel color="purple">Stable release</TagLabel>;
  else if (gitOpsApp && gitOpsState === "updating") verdict = <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>Catalog updating</StatusLabel>;
  else if (gitOpsApp && gitOpsState === "installing") verdict = <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>OLM installing</StatusLabel>;
  else if (gitOpsApp && gitOpsState === "pending") verdict = <StatusLabel status="info" icon={<ArrowCircleUpIcon />}>Install pending</StatusLabel>;
  else if (gitOpsApp && csvInstalling) verdict = <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>Installing</StatusLabel>;
  else if (gitOpsApp && gitOpsState === "installed" && csv.phase === "Succeeded") verdict = <StatusLabel status="success">Matches Git</StatusLabel>;
  else if (gitOpsApp && gitOpsState === "installed") verdict = <StatusLabel status="warning">Operator not ready</StatusLabel>;
  else if (gitOpsApp) verdict = <StatusLabel status="warning">Not checked</StatusLabel>;
  else if (updateAvailable === true) verdict = <StatusLabel status="info" icon={<ArrowCircleUpIcon />}>Update available</StatusLabel>;
  else if (csvInstalling) verdict = <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>Installing</StatusLabel>;
  else if (updateAvailable === false && csv.phase === "Succeeded") verdict = <StatusLabel status="success">Up to date</StatusLabel>;
  else if (updateAvailable === false) verdict = <StatusLabel status="warning">Operator not ready</StatusLabel>;
  else verdict = <StatusLabel status="warning">Not checked</StatusLabel>;

  // A newer release line than the installed stream (e.g. rhoai-3.7 while on rhoai-3.6).
  const installedLine = tagLine(nightly?.installed?.tag);
  const newestLine = tagLine(latestOverall?.tag);
  const newerLine = onNightly && installedLine && newestLine
    && (newestLine[0] > installedLine[0] || (newestLine[0] === installedLine[0] && newestLine[1] > installedLine[1]))
    ? latestOverall?.tag : undefined;

  let primary: React.ReactNode = null;
  if (gitOpsApp && installed && onNightly) {
    primary = catalogBuild ? (
      <TooltipButton
        variant={gitOpsState === "pending" || csv.phase === "Failed" ? "primary" : "secondary"}
        onClick={() => onUpdate(catalogBuild)}
        disabledReason={actionReason ?? gitOpsInstallWaitReason(gitOpsState)}
      >
        {gitOpsState === "installed" ? "Install from the catalog again" : "Install the build the catalog serves"}
      </TooltipButton>
    ) : null;
  } else if (!installed && gitOpsApp) {
    // Nothing installed yet, but Git already names the build: install that one.
    primary = (
      <TooltipButton
        variant="primary"
        onClick={() => catalogBuild && onUpdate(catalogBuild)}
        disabledReason={actionReason ?? gitOpsInstallWaitReason(gitOpsState) ?? (catalogBuild ? null : "The nightly catalog has no image in Git.")}
      >
        Install the build the catalog serves
      </TooltipButton>
    );
  } else if (!installed) {
    // Before the setup the newest build can't be read yet; show the action anyway, with the reason.
    primary = (
      <TooltipButton
        variant="primary"
        onClick={() => target && onUpdate(target)}
        disabledReason={actionReason ?? (target ? null : latestLoading ? "Looking up the latest nightly..." : "The latest nightly could not be read from Quay; choose a build below.")}
      >
        Install latest nightly{target?.tag ? ` (${target.tag})` : ""}
      </TooltipButton>
    );
  } else if (!onNightly) {
    primary = target ? (
      <TooltipButton variant="primary" onClick={() => onUpdate(target)} disabledReason={actionReason}>
        {subscriptionMissing ? "Update to latest nightly" : "Switch to latest nightly"}{target.tag ? ` (${target.tag})` : ""}
      </TooltipButton>
    ) : null;
  } else if (updateAvailable === true && target) {
    primary = (
      <TooltipButton variant="primary" onClick={() => onUpdate(target)} disabledReason={actionReason}>
        Update to latest
      </TooltipButton>
    );
  } else if (updateAvailable === undefined && target) {
    primary = (
      <TooltipButton variant="secondary" onClick={() => onUpdate(target)} disabledReason={actionReason}>
        Update to latest {target.tag}
      </TooltipButton>
    );
  }

  const subtle = (text: React.ReactNode) => <span className="pf-v6-u-text-color-subtle">{text}</span>;
  const latestRow = (() => {
    if (onNightly && updateAvailable === false) return subtle(`Same as ${gitOpsApp ? "in Git" : "installed"}. You have the newest ${nightly?.installed?.tag ?? ""} build.`);
    if (target) return <BuildSummary build={target} />;
    if (nightly?.error) return subtle(nightly.error);
    if (latestLoading) return <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}><Spinner size="sm" aria-label="Checking Quay" />{subtle("Checking Quay...")}</Flex>;
    if (latest?.error) return subtle(`Could not check Quay: ${latest.error}`);
    if (!prerequisitesMet) return subtle("Available after the cluster setup");
    return subtle("Unknown");
  })();

  return (
    <Card>
      <CardHeader actions={{ actions: verdict, hasNoOffset: true }}>
        <CardTitle>
          <Title headingLevel="h2" size="lg">RHOAI on this cluster</Title>
        </CardTitle>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <DescriptionList
              isCompact
              orientation={{ md: "horizontal" }}
              horizontalTermWidthModifier={{ md: "13ch" }}
              aria-label="Installed and latest builds"
            >
              <DescriptionListGroup>
                <DescriptionListTerm>Installed</DescriptionListTerm>
                <DescriptionListDescription>
                  {!installed ? (
                    <span>Nothing yet: the RHOAI operator is not installed.</span>
                  ) : gitOpsApp && csvMissing ? (
                    <span>Nothing yet: {gitOpsState === "installing" ? "OLM is installing the build Git set." : "no operator version is installed."}</span>
                  ) : gitOpsApp && onNightly && gitOpsState === "updating" ? (
                    <span>
                      <strong>{csv.name}</strong>
                      {subtle(" (the catalog is still switching to the image Git set)")}
                    </span>
                  ) : gitOpsApp && onNightly && gitOpsState === "pending" ? (
                    <span>
                      <strong>{csv.name}</strong>
                      {subtle(" (not the build the catalog serves)")}
                    </span>
                  ) : onNightly && nightly?.installed ? (
                    <BuildSummary build={nightly.installed} />
                  ) : onNightly ? (
                    <code className="pf-v6-u-text-break-word">{status.catalogSource.image || "nightly catalog (image unknown)"}</code>
                  ) : subscriptionMissing ? (
                    <span>
                      <strong>{csv.version ? `RHOAI ${csv.version}` : csv.name}</strong>
                      {subtle(" (its Subscription is missing)")}
                    </span>
                  ) : (
                    <span>
                      <strong>{csv.version ? `RHOAI ${csv.version}` : csv.name}</strong>
                      {subtle(` from ${status.subscription.source} / ${status.subscription.channel}`)}
                    </span>
                  )}
                </DescriptionListDescription>
              </DescriptionListGroup>
              {gitOpsApp && (onNightly || !installed) && catalogBuild && (
                <DescriptionListGroup>
                  <DescriptionListTerm>In Git</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Stack>
                      <StackItem><BuildSummary build={catalogBuild} /></StackItem>
                      <StackItem>
                        {subtle(`Catalog image set by ${catalogImageSetter(status)}${serves?.csv ? `; serves ${serves.csv}` : ""}`)}
                      </StackItem>
                    </Stack>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              <DescriptionListGroup>
                <DescriptionListTerm>{onNightly ? "Latest" : "Latest nightly"}</DescriptionListTerm>
                <DescriptionListDescription>{latestRow}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Operator</DescriptionListTerm>
                <DescriptionListDescription>
                  {installed ? (
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                      <FlexItem>{csv.name && csv.phase !== "Not Found" ? csv.name : "rhods-operator"}</FlexItem>
                      <FlexItem><PhaseLabel phase={csv.phase} /></FlexItem>
                    </Flex>
                  ) : (
                    subtle("rhods-operator: not installed")
                  )}
                </DescriptionListDescription>
              </DescriptionListGroup>
              {installed && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Catalog</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                      <FlexItem>
                        {!subscriptionMissing
                          ? `${status.subscription.source} / ${status.subscription.channel || "no channel"}`
                          : status.catalogSource.exists
                            ? <>{status.catalogSource.name}{subtle(" (no Subscription uses it)")}</>
                            : subtle("No Subscription and no nightly catalog")}
                      </FlexItem>
                      {(onNightly || subscriptionMissing) && status.catalogSource.exists && (
                        <FlexItem>
                          {status.catalogSource.state === "READY"
                            ? <StatusLabel status="success">Catalog ready</StatusLabel>
                            : <StatusLabel status="warning">{status.catalogSource.state || "Catalog state unknown"}</StatusLabel>}
                        </FlexItem>
                      )}
                    </Flex>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
            </DescriptionList>
          </StackItem>

          {newerLine && (
            <StackItem>
              <Alert
                variant="info"
                isInline
                isPlain
                component="p"
                title={`A newer release line is available: ${newerLine}`}
                actionLinks={gitOpsApp ? undefined : <AlertActionLink onClick={onChooseBuild}>Choose it below</AlertActionLink>}
              >
                {gitOpsApp ? "To move to it, change the catalog image in Git." : undefined}
              </Alert>
            </StackItem>
          )}

          {gitOpsApp && onNightly && updateAvailable === true && target && (
            <StackItem>
              <Alert variant="info" isInline component="p" title={`A newer ${target.tag ?? ""} nightly exists; Git does not use it yet`.replace("  ", " ")}>
                <Stack hasGutter>
                  <StackItem>
                    {catalogManagedByACM(status) ? (
                      <>
                        An ACM policy (config-policy-controller) sets the catalog image. To move to this build, change the image in the
                        policy to the reference below, then install the build the catalog serves once the policy has applied it.
                      </>
                    ) : (
                      <>
                        {catalogImageSetter(status)} sets the catalog image in Git. To move to this build, change the image there to
                        the reference below, then install the build the catalog serves once Argo CD has synced it.
                      </>
                    )}
                  </StackItem>
                  <StackItem>
                    <ClipboardCopy isReadOnly hoverTip="Copy" clickTip="Copied" variant="inline-compact">{target.image}</ClipboardCopy>
                  </StackItem>
                </Stack>
              </Alert>
            </StackItem>
          )}

          {csv.phase === "Failed" && (
            <StackItem>
              <Alert
                variant="danger"
                isInline
                component="p"
                title="The operator is Failed"
                actionLinks={
                  <>
                    {!gitOpsApp && !gitOpsProfile && <AlertActionLink onClick={onRedeploy}>Re-deploy the same version</AlertActionLink>}
                    <AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>
                    {safeConsoleURL && (
                      <AlertActionLink
                        component="a"
                        href={`${safeConsoleURL}/k8s/ns/redhat-ods-operator/operators.coreos.com~v1alpha1~ClusterServiceVersion`}
                        target="_blank"
                        rel="noopener noreferrer"
                        icon={<ExternalLinkAltIcon />}
                        iconPosition="end"
                      >
                        View in console
                      </AlertActionLink>
                    )}
                  </>
                }
              >
                {gitOpsApp
                  ? "Recommended: install the build the catalog serves again, which removes the failed version first. If that build is the broken one, change the catalog image in Git to a newer nightly first."
                  : <>Recommended: update to {onNightly && updateAvailable === false ? "another" : "the latest"} nightly; a new build
                is the usual fix for a broken one. Update removes the failed version first.</>}
              </Alert>
            </StackItem>
          )}
          {subscriptionMissing && (
            <StackItem>
              <Alert
                variant="warning"
                isInline
                component="p"
                title="The operator has no Subscription"
                actionLinks={<AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>}
              >
                {csv.name || "The operator"} is installed, but its Subscription is missing, so OLM does not update or repair it.
                This usually means an Update or Reinstall was interrupted. Update or Reinstall recreates the Subscription with
                the settings recorded before the last operation.
              </Alert>
            </StackItem>
          )}
          {status.subscription.source && csv.phase === "Not Found" && (
            <StackItem>
              <Alert variant="warning" isInline component="p" title="Subscribed, but no operator version is installed">
                OLM has not installed a CSV for the Subscription ({status.subscription.state || "no state"}).
                {gitOpsState === "installing"
                  ? ` OLM is installing the build Git set${status.installPlan ? ` (InstallPlan ${status.installPlan.name} is ${status.installPlan.phase})` : ""}; wait for it to finish. Diagnostics shows why if it is stuck.`
                  : gitOpsApp
                    ? " Installing the build the catalog serves recreates the Subscription, which usually unblocks OLM; Diagnostics shows why it is stuck."
                    : " Update or Reinstall creates a fresh install; Diagnostics shows why OLM is stuck."}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </CardBody>
      <CardFooter>
        <Flex columnGap={{ default: "columnGapLg" }} rowGap={{ default: "rowGapMd" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
          {primary && <FlexItem>{primary}</FlexItem>}
          {prerequisitesMet && !gitOpsApp && !gitOpsProfile && (
            <FlexItem>
              <Button variant="link" isInline onClick={onChooseBuild}>Choose another build</Button>
            </FlexItem>
          )}
          {previewBuild && onPreview && (
            <FlexItem>
              <Button variant="link" isInline icon={<SearchIcon />} onClick={() => onPreview(previewBuild.image)}>
                View build contents
              </Button>
            </FlexItem>
          )}
          {!prerequisitesMet && (
            <FlexItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">Finish the cluster setup above first.</Content>
            </FlexItem>
          )}
        </Flex>
      </CardFooter>
    </Card>
  );
};

interface ClusterSetupCardProps {
  status: StatusResponse;
  onStatusRefresh: () => void;
  onShowInstructions: () => void;
  /** Once the setup is done it is a collapsed card at the end of the page. */
  collapsible?: boolean;
  /** Show the state only (the gitops profile, which does not set the cluster up). */
  readOnly?: boolean;
}

/**
 * The two one-time prerequisites, pull secret and image mirror, as rows of
 * one card. While either is missing it is open and comes first; once both
 * are ready it is an expandable card that shows "Ready".
 */
export const ClusterSetupCard: React.FC<ClusterSetupCardProps> = ({ status, onStatusRefresh, onShowInstructions, collapsible = false, readOnly = false }) => {
  const [expanded, setExpanded] = useState(false);
  const ready = setupComplete(status);
  const done = (status.pullSecret.exists && status.pullSecret.valid ? 1 : 0) + (status.imageMirror.exists ? 1 : 0);
  const titleId = collapsible ? "cluster-setup-title" : "setup-title";
  const body = (
    <CardBody>
      <Stack hasGutter>
        <StackItem>
          <Content component="p" className="pf-v6-u-text-color-subtle">
            Nightly builds come from quay.io/rhoai. The cluster needs these two once, before the first install.
            {readOnly && " In the gitops profile the updater only shows them: set them up in Git."}
          </Content>
        </StackItem>
        <StackItem>
          <CardList aria-label="Cluster setup">
            <CardListItem labelledBy="setup-pull-secret">
              <PullSecretSetup pullSecret={status.pullSecret} onStatusRefresh={onStatusRefresh} titleId="setup-pull-secret" readOnly={readOnly} />
            </CardListItem>
            <CardListItem labelledBy="setup-image-mirror">
              <Stack hasGutter>
                <StackItem>
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                    <FlexItem><strong id="setup-image-mirror">Image mirror (IDMS)</strong></FlexItem>
                    <FlexItem>
                      {status.imageMirror.exists
                        ? <StatusLabel status="success">Ready</StatusLabel>
                        : <StatusLabel status="danger">Missing</StatusLabel>}
                    </FlexItem>
                  </Flex>
                  <Content component="p" className="pf-v6-u-text-color-subtle">
                    Redirects registry.redhat.io/rhoai image pulls to quay.io/rhoai, where nightly images are published.
                  </Content>
                </StackItem>
                <StackItem>
                  {status.imageMirror.exists ? (
                    <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "8ch" }}>
                      <DescriptionListGroup>
                        <DescriptionListTerm>Name</DescriptionListTerm>
                        <DescriptionListDescription>{status.imageMirror.name || "unknown"}</DescriptionListDescription>
                      </DescriptionListGroup>
                      <DescriptionListGroup>
                        <DescriptionListTerm>Source</DescriptionListTerm>
                        <DescriptionListDescription>{status.imageMirror.source || "registry.redhat.io/rhoai"}</DescriptionListDescription>
                      </DescriptionListGroup>
                    </DescriptionList>
                  ) : (
                    <Button variant="secondary" onClick={onShowInstructions}>How to create the image mirror</Button>
                  )}
                </StackItem>
              </Stack>
            </CardListItem>
          </CardList>
        </StackItem>
      </Stack>
    </CardBody>
  );
  const label = ready
    ? <StatusLabel status="success">Ready</StatusLabel>
    : <StatusLabel status="warning">{`${done} of 2 ready`}</StatusLabel>;

  if (!collapsible) {
    return (
      <Card aria-labelledby={titleId}>
        <CardHeader actions={{ actions: label, hasNoOffset: true }}>
          <CardTitle><Title headingLevel="h2" size="lg" id={titleId}>One-time cluster setup</Title></CardTitle>
        </CardHeader>
        {body}
      </Card>
    );
  }
  return (
    <Card isExpanded={expanded} aria-labelledby={titleId}>
      <CardHeader
        onExpand={() => setExpanded(!expanded)}
        toggleButtonProps={{ id: "cluster-setup-toggle", "aria-label": "Cluster setup details", "aria-expanded": expanded, "aria-labelledby": `cluster-setup-toggle ${titleId}` }}
        actions={{ actions: label, hasNoOffset: true }}
      >
        <CardTitle><Title headingLevel="h2" size="lg" id={titleId}>Cluster setup</Title></CardTitle>
      </CardHeader>
      <CardExpandableContent>{body}</CardExpandableContent>
    </Card>
  );
};
