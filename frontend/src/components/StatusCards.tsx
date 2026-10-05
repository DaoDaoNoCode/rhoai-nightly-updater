import React from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Card,
  CardBody,
  CardFooter,
  CardHeader,
  CardTitle,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  GridItem,
  Icon,
  Label,
  List,
  ListItem,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import ArrowCircleUpIcon from "@patternfly/react-icons/dist/esm/icons/arrow-circle-up-icon";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import CloneIcon from "@patternfly/react-icons/dist/esm/icons/clone-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import { Link, useNavigate } from "react-router-dom";
import type { LatestNightlyResponse, NightlyBuild, StatusResponse } from "../types";
import { isOnNightly, tagLine } from "../build";
import { operatorInstalled } from "../utils";
import { BuildSummary } from "./BuildSummary";
import { TooltipButton } from "./TooltipButton";

/** The CSV phase as a label (Succeeded green, Failed red, in progress blue). */
export function PhaseLabel({ phase }: { phase: string }): React.ReactElement {
  switch (phase) {
    case "Succeeded":
      return <Label isCompact color="green" icon={<CheckCircleIcon />}>Succeeded</Label>;
    case "Failed":
      return <Label isCompact color="red" icon={<ExclamationCircleIcon />}>Failed</Label>;
    case "Not Found":
      return <Label isCompact color="grey">Not installed</Label>;
    case "Installing":
    case "Replacing":
    case "Pending":
    case "InstallReady":
      return <Label isCompact color="blue" icon={<Spinner size="sm" aria-hidden="true" />}>{phase}</Label>;
    default:
      return <Label isCompact color="orange" icon={<ExclamationTriangleIcon />}>{phase || "Unknown"}</Label>;
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
  onPreview: (image: string) => void;
  onRedeploy: () => void;
}

type Verdict = { text: string; color: "blue" | "green" | "grey" | "orange" | "purple"; icon?: React.ReactNode };

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
  const setupReason = !prerequisitesMet ? "Finish the one-time cluster setup first (pull secret and image mirror)." : null;
  const actionReason = blocker ?? setupReason;

  let verdict: Verdict;
  if (!installed) verdict = { text: "Not installed", color: "grey" };
  else if (!onNightly) verdict = { text: "Stable release", color: "purple" };
  else if (updateAvailable === true) verdict = { text: "Update available", color: "blue", icon: <ArrowCircleUpIcon /> };
  else if (updateAvailable === false) verdict = { text: "Up to date", color: "green", icon: <CheckCircleIcon /> };
  else verdict = { text: "Not checked", color: "orange", icon: <ExclamationTriangleIcon /> };

  // A newer release line than the installed stream (e.g. rhoai-3.7 while on rhoai-3.6).
  const installedLine = tagLine(nightly?.installed?.tag);
  const newestLine = tagLine(latestOverall?.tag);
  const newerLine = onNightly && installedLine && newestLine
    && (newestLine[0] > installedLine[0] || (newestLine[0] === installedLine[0] && newestLine[1] > installedLine[1]))
    ? latestOverall?.tag : undefined;

  let primary: React.ReactNode = null;
  if (!installed) {
    primary = target ? (
      <TooltipButton variant="primary" onClick={() => onUpdate(target)} disabledReason={actionReason}>
        Install latest nightly{target.tag ? ` (${target.tag})` : ""}
      </TooltipButton>
    ) : null;
  } else if (!onNightly) {
    primary = target ? (
      <TooltipButton variant="primary" onClick={() => onUpdate(target)} disabledReason={actionReason}>
        Switch to latest nightly{target.tag ? ` (${target.tag})` : ""}
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

  const latestRow = (() => {
    if (target) {
      return <BuildSummary build={target} />;
    }
    if (nightly?.error) return <span className="rhoai-subtle">{nightly.error}</span>;
    if (latestLoading) return <Flex gap={{ default: "gapSm" }}><Spinner size="sm" aria-label="Checking Quay" /> <span className="rhoai-subtle">Checking Quay...</span></Flex>;
    if (latest?.error) return <span className="rhoai-subtle">Could not check Quay: {latest.error}</span>;
    if (!prerequisitesMet) return <span className="rhoai-subtle">Available after the cluster setup</span>;
    return <span className="rhoai-subtle">Unknown</span>;
  })();

  return (
    <Card>
      <CardHeader
        actions={{
          actions: (
            <Label color={verdict.color} icon={verdict.icon}>{verdict.text}</Label>
          ),
          hasNoOffset: true,
        }}
      >
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
                  ) : onNightly && nightly?.installed ? (
                    <BuildSummary build={nightly.installed} />
                  ) : onNightly ? (
                    <span className="rhoai-build-id">{status.catalogSource.image || "nightly catalog (image unknown)"}</span>
                  ) : (
                    <span>
                      <strong>{csv.version ? `RHOAI ${csv.version}` : csv.name}</strong>
                      <span className="rhoai-subtle"> from {status.subscription.source} / {status.subscription.channel}</span>
                    </span>
                  )}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>{onNightly ? "Latest" : "Latest nightly"}</DescriptionListTerm>
                <DescriptionListDescription>
                  {latestRow}
                  {onNightly && updateAvailable === false && (
                    <span className="rhoai-subtle"> (same build)</span>
                  )}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Operator</DescriptionListTerm>
                <DescriptionListDescription>
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                    <FlexItem>{csv.name && csv.phase !== "Not Found" ? csv.name : "rhods-operator"}</FlexItem>
                    <FlexItem><PhaseLabel phase={csv.phase} /></FlexItem>
                    {installed && <FlexItem><Link to="/components">Components</Link></FlexItem>}
                  </Flex>
                </DescriptionListDescription>
              </DescriptionListGroup>
              {installed && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Catalog</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                      <FlexItem>{status.subscription.source || "none"} / {status.subscription.channel || "no channel"}</FlexItem>
                      {onNightly && status.catalogSource.exists && (
                        <FlexItem>
                          {status.catalogSource.state === "READY"
                            ? <Label isCompact variant="outline" color="green">Catalog ready</Label>
                            : <Label isCompact variant="outline" color="orange">{status.catalogSource.state || "Catalog state unknown"}</Label>}
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
              <Content component="small">
                A newer release line is available: <strong>{newerLine}</strong>.{" "}
                <Button variant="link" isInline onClick={onChooseBuild}>Choose it below</Button>
              </Content>
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
                    <AlertActionLink onClick={onRedeploy}>Re-deploy the same version</AlertActionLink>
                    <AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>
                    {safeConsoleURL && (
                      <AlertActionLink
                        component="a"
                        href={`${safeConsoleURL}/k8s/ns/redhat-ods-operator/operators.coreos.com~v1alpha1~ClusterServiceVersion`}
                        target="_blank"
                        rel="noopener noreferrer"
                      >
                        View in console <ExternalLinkAltIcon />
                      </AlertActionLink>
                    )}
                  </>
                }
              >
                Recommended: update to {onNightly && updateAvailable === false ? "another" : "the latest"} nightly; a new build
                is the usual fix for a broken one. Update removes the failed version first.
              </Alert>
            </StackItem>
          )}
          {status.subscription.source && csv.phase === "Not Found" && (
            <StackItem>
              <Alert variant="warning" isInline component="p" title="Subscribed, but no operator version is installed">
                OLM has not installed a CSV for the Subscription ({status.subscription.state || "no state"}). Update or Reinstall
                creates a fresh install; Diagnostics shows why OLM is stuck.
              </Alert>
            </StackItem>
          )}
        </Stack>
      </CardBody>
      <CardFooter>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
          {primary && <FlexItem>{primary}</FlexItem>}
          {onNightly && updateAvailable === false && (
            <FlexItem><Content component="small">You have the newest {nightly?.installed?.tag} build.</Content></FlexItem>
          )}
          <FlexItem>
            <Button variant="link" isInline onClick={onChooseBuild}>Choose another build</Button>
          </FlexItem>
          {target && (
            <FlexItem>
              <Button variant="link" isInline icon={<SearchIcon />} onClick={() => onPreview(target.image)}>
                What&apos;s in the latest build
              </Button>
            </FlexItem>
          )}
        </Flex>
      </CardFooter>
    </Card>
  );
};

/** Shown instead of the hero while the first status request is in flight. */
export const StatusLoading: React.FC = () => (
  <Card>
    <CardBody>
      <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} role="status">
        <FlexItem><Spinner size="lg" aria-label="Loading cluster status" /></FlexItem>
        <FlexItem>Loading cluster status...</FlexItem>
      </Flex>
    </CardBody>
  </Card>
);

interface SetupCardsProps {
  status: StatusResponse;
  /** The pull secret card (it owns its own form state). */
  pullSecretCard: React.ReactNode;
  onShowInstructions: () => void;
}

/** The two one-time prerequisites side by side: pull secret and image mirror. */
export const SetupCards: React.FC<SetupCardsProps> = ({ status, pullSecretCard, onShowInstructions }) => (
  <>
    {pullSecretCard}
    <GridItem lg={6} md={6} sm={12}>
      <Card isFullHeight isCompact>
        <CardHeader>
          <CardTitle>
            <Flex alignItems={{ default: "alignItemsCenter" }} justifyContent={{ default: "justifyContentSpaceBetween" }} flexWrap={{ default: "nowrap" }}>
              <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                <FlexItem><Icon><CloneIcon /></Icon></FlexItem>
                <FlexItem>Image mirror (IDMS)</FlexItem>
              </Flex>
              <FlexItem>
                {status.imageMirror.exists
                  ? <Label color="green" variant="outline" isCompact>Ready</Label>
                  : <Label status="danger" variant="outline" isCompact>Missing</Label>}
              </FlexItem>
            </Flex>
          </CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="small">
                Redirects registry.redhat.io/rhoai image pulls to quay.io/rhoai, where nightly images are published.
              </Content>
            </StackItem>
            {status.imageMirror.exists ? (
              <StackItem>
                <List isPlain>
                  <ListItem><Content component="small">Name: {status.imageMirror.name || "unknown"}</Content></ListItem>
                  <ListItem><Content component="small">Source: {status.imageMirror.source || "registry.redhat.io/rhoai"}</Content></ListItem>
                </List>
              </StackItem>
            ) : (
              <StackItem>
                <Button variant="link" isInline onClick={onShowInstructions}>How to create the image mirror</Button>
              </StackItem>
            )}
          </Stack>
        </CardBody>
      </Card>
    </GridItem>
  </>
);
