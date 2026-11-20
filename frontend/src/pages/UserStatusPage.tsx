import React, { useEffect } from "react";
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Grid,
  GridItem,
  PageSection,
  Title,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import { useNavigate } from "react-router-dom";
import type { ClusterSummary, SummaryComponent } from "../types";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { StatusLabel, type StatusKind } from "../components/StatusLabel";
import { RelativeTime } from "../components/RelativeTime";
import { nightlyProblemText, useSummary } from "../hooks/useSummary";

/** Operator CSV phases that mean OLM is still working on it. */
const INSTALLING_PHASES = new Set(["Pending", "InstallReady", "Installing", "Replacing", "Upgrading"]);

function phaseLabel(rhoai: ClusterSummary["rhoai"]): React.ReactNode {
  if (!rhoai.installed) return <StatusLabel status="neutral">Not installed</StatusLabel>;
  const phase = rhoai.phase || "Unknown";
  const kind: StatusKind = phase === "Succeeded" ? "success" : phase === "Failed" ? "danger" : INSTALLING_PHASES.has(phase) ? "progress" : "warning";
  return <StatusLabel status={kind}>{phase}</StatusLabel>;
}

/** Ready means something only for a component the operator runs. */
function componentReady(c: SummaryComponent): React.ReactNode {
  if (c.managementState !== "Managed") return <span className="pf-v6-u-text-color-subtle">Not run by RHOAI</span>;
  return c.ready ? <StatusLabel status="success">Ready</StatusLabel> : <StatusLabel status="warning">Not ready</StatusLabel>;
}

const Row: React.FC<{ term: string; children: React.ReactNode }> = ({ term, children }) => (
  <DescriptionListGroup>
    <DescriptionListTerm>{term}</DescriptionListTerm>
    <DescriptionListDescription>{children}</DescriptionListDescription>
  </DescriptionListGroup>
);

const NotKnown: React.FC = () => <span className="pf-v6-u-text-color-subtle">Not known</span>;

const RHOAICard: React.FC<{ rhoai: ClusterSummary["rhoai"] }> = ({ rhoai }) => (
  <Card isFullHeight>
    <CardHeader><CardTitle><Title headingLevel="h2" size="lg">RHOAI</Title></CardTitle></CardHeader>
    <CardBody>
      <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="RHOAI">
        <Row term="Operator">{phaseLabel(rhoai)}</Row>
        {rhoai.installed && <Row term="Version">{rhoai.version || <NotKnown />}</Row>}
        {rhoai.installed && <Row term="Channel">{rhoai.channel || <NotKnown />}</Row>}
      </DescriptionList>
    </CardBody>
  </Card>
);

const NightlyCard: React.FC<{ nightly: ClusterSummary["nightly"] }> = ({ nightly }) => {
  const navigate = useNavigate();
  const installed = nightly.installedBuild;
  const tag = installed?.tag ?? "";
  const problem = nightlyProblemText(nightly.problem);
  return (
    <Card isFullHeight>
      <CardHeader actions={{ actions: <Button variant="link" isInline onClick={() => navigate("/builds")}>Open Build Explorer</Button>, hasNoOffset: true }}>
        <CardTitle><Title headingLevel="h2" size="lg">Nightly build</Title></CardTitle>
      </CardHeader>
      <CardBody>
        {installed ? (
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "12ch" }} aria-label="Nightly build">
            <Row term="Installed">
              <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
                {tag && <FlexItem><strong>{tag}</strong></FlexItem>}
                {installed.buildDate && <FlexItem><RelativeTime date={installed.buildDate} prefix="built " size="inherit" /></FlexItem>}
              </Flex>
            </Row>
            <Row term="Newer build">
              {problem
                ? <StatusLabel status="warning">{problem}</StatusLabel>
                : nightly.newerAvailable
                  ? <StatusLabel status="info">A newer {tag || "nightly"} build exists</StatusLabel>
                  : <StatusLabel status="success">This is the newest build</StatusLabel>}
            </Row>
          </DescriptionList>
        ) : problem ? (
          <Content component="p" className="pf-v6-u-text-color-subtle">{problem}</Content>
        ) : (
          <Content component="p" className="pf-v6-u-text-color-subtle">RHOAI on this cluster was not installed from a nightly build.</Content>
        )}
      </CardBody>
    </Card>
  );
};

const DSCCard: React.FC<{ dsc: ClusterSummary["dsc"] }> = ({ dsc }) => {
  const components = [...(dsc.components ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  return (
    <Card>
      <CardHeader>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><CardTitle><Title headingLevel="h2" size="lg">DataScienceCluster</Title></CardTitle></FlexItem>
          {dsc.exists && <FlexItem>{dsc.ready ? <StatusLabel status="success">Ready</StatusLabel> : <StatusLabel status="warning">Not ready</StatusLabel>}</FlexItem>}
        </Flex>
      </CardHeader>
      <CardBody>
        {!dsc.exists ? (
          <Content component="p" className="pf-v6-u-text-color-subtle">There is no DataScienceCluster. A cluster-admin creates it.</Content>
        ) : components.length === 0 ? (
          <Content component="p" className="pf-v6-u-text-color-subtle">The DataScienceCluster reports no components.</Content>
        ) : (
          <Table aria-label="DataScienceCluster components" variant="compact">
            <Thead>
              <Tr>
                <Th width={40}>Component</Th>
                <Th width={30}>Management state</Th>
                <Th width={30}>Ready</Th>
              </Tr>
            </Thead>
            <Tbody>
              {components.map((c) => (
                <Tr key={c.name}>
                  <Td dataLabel="Component">{c.name}</Td>
                  <Td dataLabel="Management state">{c.managementState || <NotKnown />}</Td>
                  <Td dataLabel="Ready">{componentReady(c)}</Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </CardBody>
    </Card>
  );
};

/**
 * Status for a user who is not a cluster-admin: the read-only summary
 * (GET /api/summary) and nothing else. No activity, pull secret, update or
 * reinstall, Argo CD details, and no request only cluster-admins may make.
 */
export const UserStatusPage: React.FC = () => {
  const { summary, loading, error, lastRefreshed, refresh } = useSummary();
  useEffect(() => {
    document.title = summary?.rhoai.installed
      ? `${summary.rhoai.version || "RHOAI"} (${summary.rhoai.phase || "unknown"}) · RHOAI Nightly Updater`
      : "RHOAI Nightly Updater";
  }, [summary]);

  return (
    <>
      <PageHeader
        title="Status"
        description="What runs on this cluster. Only cluster-admins can change it."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={refresh}
        details={summary?.updaterVersion ? (
          <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "14ch" }} aria-label="Updater">
            <Row term="Updater version">{summary.updaterVersion}</Row>
          </DescriptionList>
        ) : undefined}
      />
      {!summary && error && <PageErrorState error={error} title="Can't load the cluster summary" onRetry={refresh} diagnostics={false} />}
      {!summary && !error && <PageLoading title="Loading the cluster summary" />}
      {summary && error && <LoadErrorAlert error={error} genericTitle="Could not refresh the cluster summary" onRetry={refresh} stale />}
      {summary && (
        <PageSection isFilled>
          <Grid hasGutter>
            <GridItem md={6}><RHOAICard rhoai={summary.rhoai} /></GridItem>
            <GridItem md={6}><NightlyCard nightly={summary.nightly} /></GridItem>
            <GridItem span={12}><DSCCard dsc={summary.dsc} /></GridItem>
          </Grid>
        </PageSection>
      )}
    </>
  );
};
