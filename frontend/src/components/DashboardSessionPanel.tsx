import React from "react";
import {
  Alert,
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
  List,
  ListItem,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { DashboardOverride } from "../types";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel, TagLabel } from "./StatusLabel";
import { TooltipButton } from "./TooltipButton";

export function flavorName(flavor?: string): string {
  return flavor === "odh" ? "ODH build (OpenShift CI)" : flavor === "rhoai" ? "RHOAI build (Konflux)" : "";
}

/** "PR #222", "Latest main", or a fallback when the session was not recorded. */
export function sessionTitle(o: Pick<DashboardOverride, "mode" | "prNumber" | "sessionRecorded" | "operatorPaused">): string {
  if (o.mode === "pr" && o.prNumber) return `PR #${o.prNumber}`;
  if (o.mode === "main") return "Latest main";
  return o.operatorPaused ? "dashboard-operator paused" : "Custom dashboard images";
}

const When: React.FC<{ at?: string }> = ({ at }) => (at ? <RelativeTime date={at} size="inherit" /> : <>unknown</>);

/**
 * Titled alerts for the backend's override warnings (B4). The backend sends
 * full sentences in a fixed order (deletion blocked, stale release, paused
 * outside the tool); the flags say which of them are present, so each gets
 * the right title and severity without parsing the text.
 */
export function overrideAlerts(o: DashboardOverride): { title: string; body: string; variant: "danger" | "warning"; list?: string[] }[] {
  const warnings = [...(o.warnings ?? [])];
  const take = (fallback: string) => warnings.shift() ?? fallback;
  const out: { title: string; body: string; variant: "danger" | "warning"; list?: string[] }[] = [];
  if (o.dashboardDeleting) {
    out.push({
      title: "Dashboard deletion is blocked until you revert",
      body: take("A Dashboard CR is being deleted while dashboard-operator is paused; only dashboard-operator can finish the deletion. Revert to default to resume it."),
      variant: "danger",
    });
  }
  if (o.stale && o.operatorPaused) {
    out.push({
      title: "RHOAI was updated while dashboard-operator is paused",
      body: take("The dashboard keeps its current images. Revert to default to apply the new release."),
      variant: "warning",
      list: o.staleReasons ?? undefined,
    });
  }
  if (o.operatorPaused && !o.sessionRecorded) {
    out.push({
      title: "dashboard-operator was paused outside Dashboard Dev",
      body: take("The dashboard does not follow RHOAI updates. Revert to default resumes it."),
      variant: "warning",
    });
  }
  for (const w of warnings) out.push({ title: "Warning", body: w, variant: "warning" });
  if (o.sessionError) {
    out.push({
      title: "The saved session cannot be read",
      body: `${o.sessionError}. Revert to default still works: it resumes dashboard-operator (with 1 replica when the original count is unknown).`,
      variant: "warning",
    });
  }
  return out;
}

interface DashboardSessionPanelProps {
  override: DashboardOverride;
  dashboardURL?: string;
  revertDisabledReason: string | null;
  reverting: boolean;
  onRevert: () => void;
}

/**
 * The active Dashboard Dev session (A04-1): what runs, who started it and
 * when, the build flavor, per-component sources, every warning, and a
 * prominent Revert.
 */
export const DashboardSessionPanel: React.FC<DashboardSessionPanelProps> = ({ override: o, dashboardURL, revertDisabledReason, reverting, onRevert }) => {
  const components = o.components ?? [];
  const fromBuild = components.filter((c) => c.source === "pr" || c.source === "main").length;
  const alerts = overrideAlerts(o);
  const title = sessionTitle(o);
  return (
    <Card>
      <CardHeader
        actions={{
          actions: (
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
              {o.flavor && <FlexItem><TagLabel color="blue">{flavorName(o.flavor)}</TagLabel></FlexItem>}
              {o.operatorPaused && <FlexItem><StatusLabel status="warning">Operator paused</StatusLabel></FlexItem>}
            </Flex>
          ),
          hasNoOffset: true,
        }}
      >
        <CardTitle><Title headingLevel="h2" size="lg">Dashboard Dev session active: {title}</Title></CardTitle>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Content component="p" className="pf-v6-u-text-color-subtle">Everyone using this cluster sees this dashboard until someone reverts.</Content>
          </StackItem>
          {alerts.map((a) => (
            <StackItem key={a.title}>
              <Alert component="p" variant={a.variant} isInline title={a.title}>
                {a.body}
                {a.list && a.list.length > 0 && <List>{a.list.map((r) => <ListItem key={r}>{r}</ListItem>)}</List>}
              </Alert>
            </StackItem>
          ))}
          <StackItem>
            <DescriptionList isCompact isHorizontal columnModifier={{ default: "1Col", lg: "2Col" }}>
              <DescriptionListGroup>
                <DescriptionListTerm>Started</DescriptionListTerm>
                <DescriptionListDescription>
                  {o.startedBy ? <><strong>{o.startedBy}</strong>, </> : null}<When at={o.startedAt} />
                </DescriptionListDescription>
              </DescriptionListGroup>
              {(o.updatedAt && o.updatedAt !== o.startedAt) && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Last changed</DescriptionListTerm>
                  <DescriptionListDescription>{o.updatedBy ? <><strong>{o.updatedBy}</strong>, </> : null}<When at={o.updatedAt} /></DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {o.lastAction && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Last action</DescriptionListTerm>
                  <DescriptionListDescription>
                    {o.lastAction.detail || o.lastAction.action} ({o.lastAction.by}, <When at={o.lastAction.at} />)
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {components.length > 0 && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Containers</DescriptionListTerm>
                  <DescriptionListDescription>
                    {fromBuild} of {components.length} run the {o.mode === "main" ? "main" : "PR"} build; {components.length - fromBuild} run the release image
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {(o.releaseVersion || o.releaseVersionAtStart) && (
                <DescriptionListGroup>
                  <DescriptionListTerm>RHOAI release</DescriptionListTerm>
                  <DescriptionListDescription>
                    {o.releaseVersionAtStart && o.releaseVersion && o.releaseVersionAtStart !== o.releaseVersion
                      ? `${o.releaseVersionAtStart} at start, now ${o.releaseVersion}`
                      : o.releaseVersion || o.releaseVersionAtStart}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
            </DescriptionList>
          </StackItem>
        </Stack>
      </CardBody>
      <CardFooter>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
          <FlexItem>
            <TooltipButton variant={o.dashboardDeleting ? "danger" : "primary"} onClick={onRevert} isLoading={reverting} disabledReason={revertDisabledReason}>
              Revert to default
            </TooltipButton>
          </FlexItem>
          {o.mode === "pr" && o.prNumber ? (
            <FlexItem>
              <Button variant="link" isInline component="a" href={`https://github.com/opendatahub-io/odh-dashboard/pull/${o.prNumber}`} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                PR #{o.prNumber} on GitHub
              </Button>
            </FlexItem>
          ) : null}
          {dashboardURL && (
            <FlexItem>
              <Button variant="link" isInline component="a" href={dashboardURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">
                Open the dashboard
              </Button>
            </FlexItem>
          )}
        </Flex>
      </CardFooter>
    </Card>
  );
};
