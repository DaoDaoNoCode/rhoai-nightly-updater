import React, { useCallback, useEffect, useState } from "react";
import {
  ActionList,
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Label,
  Stack,
  StackItem,
  Tooltip,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { MaaSGroupProblem, MaaSModel, MaaSPart, MaaSRequest, MaaSStatus } from "../../types";
import { AudienceFields, DatabaseChoice, KeysLostCheck, LimitFields } from "./MaaSDialogFields";
import {
  audienceProblem, audienceRequest, audienceText, createdDatabaseInSentence, databaseLabel, databaseProblem, databaseRequest, defaultAudienceForm, defaultDatabaseForm,
  defaultLimitForm, limitProblem, limitRequest, limitText,
} from "./maasForms";
import { getMaaS, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { RESOURCE_SETTLE_MAX_MS } from "../../constants";
import { ImageRef } from "../ImageRef";
import { type ConfirmSpec, type MaaSPending, type Pending, ExternalLink, WarningAlert } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ComponentAlert, CopyableCode, shellQuote } from "./UseIt";
import { LoadFailed, PartsSkeleton, RowsSkeleton } from "./Loading";
import { EnvStatusLabel } from "./resourceStatus";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, maasDashboardHref, maasPartLabelStatus } from "./featureState";

export const MAAS_POLL_MS = 5_000;

/** Something about MaaS may still change on its own: a part or a publication on its way. */
export function maasSettling(m: MaaSStatus | null): boolean {
  return !!m && (m.parts.some((p) => p.state === "waiting") || (m.items ?? []).some((i) => i.published && !i.ready && !i.partly));
}

/** GET /api/resources/maas, read again when refreshKey changes and every few seconds while something settles. */
export function useMaaSStatus(refreshKey: number, busy: boolean, enabled = true) {
  const [data, setData] = useState<MaaSStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pollRound, setPollRound] = useState(0);
  const fetchNow = useCallback(async () => {
    if (!enabled) return;
    try {
      setData(await getMaaS());
      setError(null);
    } catch (e) {
      setError(toApiError(e, "Cannot read Models as a Service").message);
    }
  }, [enabled]);
  useEffect(() => {
    setPollRound((n) => n + 1);
    void fetchNow();
  }, [fetchNow, refreshKey]);
  usePolling(fetchNow, {
    enabled: enabled && (busy || maasSettling(data)),
    restartKey: pollRound,
    delay: ({ elapsedMs }) => (elapsedMs >= RESOURCE_SETTLE_MAX_MS ? null : MAAS_POLL_MS),
  });
  return { data, error, reload: fetchNow };
}

export interface MaaSTabProps {
  /** GET /api/resources/maas; null until it has been read. */
  maas: MaaSStatus | null;
  maasError: string | null;
  /** Reads MaaS's status again (Try again on a failed read). */
  onRetry: () => void;
  /** maasState: the same as the Overview's. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  /** Opens the Models tab. */
  onModels: () => void;
}

/** This tool's MaaSSubscription and the display name it writes on it (pkg/cluster maas_subscription.go), as the dashboard lists it. */
const TOOL_SUBSCRIPTION = "rhoai-nightly-updater-subscription";
const TOOL_SUBSCRIPTION_DISPLAY = "rhoai-nightly-updater models";

/** The key-mint, model list and chat requests of the MaaS docs (validation.md), with the user's own token. */
export function maasCurl(url: string, subscription: string): string {
  return [
    `HOST=${shellQuote(url)}`,
    "# 1. A MaaS API key for the subscription (shown once: keep it)",
    `API_KEY=$(curl -sS -X POST "$HOST/maas-api/v1/api-keys" \\`,
    `  -H "Authorization: Bearer $(oc whoami -t)" -H "Content-Type: application/json" \\`,
    `  -d ${shellQuote(JSON.stringify({ name: "my-key", expiresIn: "1h", subscription }))} | jq -r .key)`,
    "# 2. The models the key may use",
    `MODEL=$(curl -sS "$HOST/maas-api/v1/models" -H "Authorization: Bearer $API_KEY" | jq -r '[.data[] | select(.ready==true)][0].id')`,
    "# 3. Chat",
    `curl -sS "$HOST/v1/chat/completions" -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \\`,
    `  -d "{\\"model\\": \\"$MODEL\\", \\"messages\\": [{\\"role\\": \\"user\\", \\"content\\": \\"Hello\\"}], \\"max_tokens\\": 50}"`,
  ].join("\n");
}

/** One platform part, with the fixes it offers on its own and the same change from a terminal. */
const PlatformPart: React.FC<{ part: MaaSPart; readOnly: boolean; reason: string | null; setPending: (p: Pending) => void }> = ({ part, readOnly, reason, setPending }) => {
  const fixes = readOnly ? [] : part.actions ?? [];
  // A "manual" part has only the commands; the others can be done here, so the commands are an alternative.
  const onlyCommands = part.state === "manual" && fixes.length === 0;
  return (
    <PartItem id={`maas-${part.id}`} title={part.title} scope="Shared by the cluster" unmanaged={!!part.notManaged}
      // Waiting for something else is not this part setting itself up: a grey "Waiting", as on Prerequisites.
      status={part.state === "waiting" ? "not-set-up" : maasPartLabelStatus(part)} statusText={part.state === "waiting" ? "Waiting" : undefined}
      actions={fixes.length > 0 || part.docUrl ? (
        <>
          {fixes.map((a) => (
            <ActionListItem key={a.id}>
              <Button variant="secondary" size="sm" isDanger={a.danger} isAriaDisabled={!!reason}
                onClick={() => setPending((a.id as string) === "change-database" ? { kind: "maas-database" } : { kind: "maas-action", action: a.id })}>{a.label}</Button>
            </ActionListItem>
          ))}
          {/* Inline: below the title (narrow pages) it lines up with the text, not indented by a button's padding. */}
          {part.docUrl && <ActionListItem><ExternalLink href={part.docUrl} isInline>Docs</ExternalLink></ActionListItem>}
        </>
      ) : undefined}>
      {part.message && <Content component="p">{part.message}</Content>}
      {part.warning && <WarningAlert text={part.warning} />}
      {part.commands && (onlyCommands
        ? <CopyableCode id={`maas-${part.id}-commands`} label="the commands for a cluster-admin" code={part.commands} />
        : (
          <FeatureSection title="Do it from the CLI instead">
            <CopyableCode id={`maas-${part.id}-commands`} label="the commands" code={part.commands} />
          </FeatureSection>
        ))}
    </PartItem>
  );
};

/** A group of the audience with no OpenShift Group, or one that could not be checked, with the commands that make it. */
export const GroupProblemAlert: React.FC<{ problem: MaaSGroupProblem; idPrefix: string }> = ({ problem, idPrefix }) => (
  <Alert variant="warning" isInline component="p" title={problem.missing ? `Group ${problem.name} does not exist` : `Cannot check group ${problem.name}`}>
    <Stack hasGutter>
      <StackItem>{problem.message}{problem.commands ? " A cluster-admin creates it and adds people to it:" : ""}</StackItem>
      {problem.commands && (
        <StackItem>
          <CopyableCode id={`${idPrefix}-group-${problem.name.replace(/[^A-Za-z0-9_-]/g, "_")}`} label={`the commands that create group ${problem.name}`} code={problem.commands} />
        </StackItem>
      )}
    </Stack>
  </Alert>
);

/** The words for a model's publication. */
export function maasPublication(i: MaaSModel): string {
  if (!i.published) return "Not published";
  if (i.partly) return i.message ?? "Partly published. Unpublish to finish removing it.";
  if (i.ready) return "Published";
  return `Published, waiting (reference ${i.phase || "pending"}, access ${i.access || "pending"}, subscription ${i.subscription || "pending"})`;
}

/** A publication MaaS has accepted. */
const publishedReady = (i: MaaSModel) => !!i.published && !i.partly && !!i.ready;

/** A model MaaS does not take (an embedding model: pkg/cluster maasEmbeddingReason). */
const notOffered = (i: MaaSModel) => !i.published && i.message === "Not offered for MaaS";

/** This tool's models and their publication: Publish / Unpublish per model. */
const PublishedModels: React.FC<MaaSTabProps> = ({ maas, busy, baseReason, readOnly, setPending, onModels }) => {
  const items = maas?.items ?? [];
  const running = (project: string, model?: string) => busy.maas?.project === project && (!busy.maas?.model || busy.maas.model === model);
  return (
    <TabSection title="Published models" id="maas-models">
      <Content component="p">
        Publish a chat model to call it through MaaS with an API key. Each user gets the token limit you set. While published, the model's direct address answers 404.
      </Content>
      {maas?.access && (
        <Stack hasGutter>
          <StackItem>
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
              <FlexItem><Content component="p"><strong>Who can use them:</strong> {audienceText(maas.access)}</Content></FlexItem>
              {!readOnly && <FlexItem><Button variant="link" isInline isAriaDisabled={!!baseReason} onClick={() => setPending({ kind: "maas-access" })}>Change who can use them</Button></FlexItem>}
            </Flex>
          </StackItem>
          {(maas.access.groupProblems ?? []).map((p) => <StackItem key={p.name}><GroupProblemAlert problem={p} idPrefix="maas-access" /></StackItem>)}
        </Stack>
      )}
      {maas && items.length === 0 ? (
        <Content component="p">
          No model of this tool yet. <Button variant="link" isInline onClick={onModels}>Set one up on the Models tab</Button>, then publish it here.
        </Content>
      ) : (
        <Table variant="compact" aria-label="This tool's models in MaaS" aria-busy={!maas}>
          <Thead>
            <Tr>
              <Th modifier="wrap" width={20}>Model</Th>
              <Th modifier="wrap" width={15}>Project</Th>
              <Th modifier="wrap" width={20}>Limit per user</Th>
              <Th modifier="wrap" width={30}>In MaaS</Th>
              <Th screenReaderText="Actions" />
            </Tr>
          </Thead>
          {!maas ? <RowsSkeleton what="this tool's models in MaaS" columns={["Model", "Project", "Limit per user", "In MaaS", "Actions"]} /> : <Tbody>
            {items.map((i) => {
              const project = i.namespace!;
              const model = i.name!;
              const reason = readOnly ? READ_ONLY_SETUP : baseReason ?? i.setupBlockedReason ?? null;
              return (
                <Tr key={`${project}/${model}`}>
                  <Td dataLabel="Model" modifier="wrap">{i.displayName || i.name}</Td>
                  <Td dataLabel="Project" modifier="wrap">{project}</Td>
                  <Td dataLabel="Limit per user" modifier="wrap">{i.published && !i.partly ? limitText(i.limit) : "—"}</Td>
                  <Td dataLabel="In MaaS" modifier="wrap">
                    <Stack>
                      {publishedReady(i) && !running(project, model)
                        // One green label: "Ready" above "Published" said the same twice.
                        ? <StackItem><EnvStatusLabel status="ready" text="Published" /></StackItem>
                        : (i.published || running(project, model))
                          ? <>
                            <StackItem><EnvStatusLabel status={running(project, model) ? (busy.maas?.action === "unpublish" ? "deleting" : "setting-up") : i.partly ? "needs-attention" : "setting-up"} /></StackItem>
                            <StackItem><Content component="small">{maasPublication(i)}</Content></StackItem>
                          </>
                          : <>
                            <StackItem><Label isCompact>{notOffered(i) ? "Not offered" : "Not published"}</Label></StackItem>
                            {/* Why Publish is off, without hovering it. */}
                            {notOffered(i) && <StackItem><Content component="small">Chat models only</Content></StackItem>}
                          </>}
                    </Stack>
                  </Td>
                  <Td dataLabel="Actions" isActionCell>
                    {!readOnly && (i.published
                      ? (
                        <ActionList isIconList>
                          {!i.partly && i.limit && <ActionListItem><Button variant="secondary" size="sm" isAriaDisabled={!!baseReason} onClick={() => setPending({ kind: "maas-limit", project, model })}>Change limit</Button></ActionListItem>}
                          <ActionListItem><Button variant="secondary" isDanger size="sm" isAriaDisabled={!!baseReason} onClick={() => setPending({ kind: "unpublish-maas", project, model })}>Unpublish</Button></ActionListItem>
                        </ActionList>
                      )
                      : reason
                        ? <Tooltip content={reason}><Button variant="secondary" size="sm" isAriaDisabled>Publish</Button></Tooltip>
                        : <Button variant="secondary" size="sm" onClick={() => setPending({ kind: "publish-maas", project, model })}>Publish</Button>)}
                  </Td>
                </Tr>
              );
            })}
          </Tbody>}
        </Table>
      )}
    </TabSection>
  );
};

/** How to call a published model: the docs' requests, and the dashboard's MaaS pages. */
const UseMaaS: React.FC<{ maas: MaaSStatus }> = ({ maas }) => {
  const href = maasDashboardHref(maas.dashboardUrl);
  return (
    <TabSection title="Use it" id="maas-use">
      <DescriptionList aria-label="How to use MaaS">
        <DescriptionListGroup>
          <DescriptionListTerm>Address</DescriptionListTerm>
          <DescriptionListDescription>{maas.url ? <ImageRef image={maas.url} what="MaaS address" truncate={false} isCode={false} /> : "Not known yet"}</DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Subscription</DescriptionListTerm>
          <DescriptionListDescription>{maas.subscription === TOOL_SUBSCRIPTION ? <>{TOOL_SUBSCRIPTION_DISPLAY} (<code>{maas.subscription}</code>)</> : <code>{maas.subscription}</code>} <Label isCompact>{audienceText(maas.access)}</Label><br />The MaaS subscription that gives access and holds the limits; you choose it when you create an API key.</DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>From a terminal</DescriptionListTerm>
          <DescriptionListDescription>
            <Stack hasGutter>
              <StackItem><Content component="p">Get an API key with your own OpenShift token, list the models it may use, and chat (the MaaS docs&apos; validation steps). The key is shown only once: MaaS keeps only its hash.</Content></StackItem>
              <StackItem><CopyableCode id="maas-curl" label="the commands" code={maasCurl(maas.url ?? "https://maas.<apps domain>", maas.subscription)} /></StackItem>
            </Stack>
          </DescriptionListDescription>
        </DescriptionListGroup>
        {href && (
          <DescriptionListGroup>
            <DescriptionListTerm>In the dashboard</DescriptionListTerm>
            <DescriptionListDescription><ExternalLink href={href} isInline>MaaS governance</ExternalLink>: subscriptions, authorization policies and models</DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="maas-next" items={[
        <>Run the commands: the chat answers, and a request without a key gets 401. More than the model&apos;s limit gets 429 (an unlimited model never does; its usage is still metered) for calls whose response reports its token usage, as the non-streaming chat above does; streaming responses report it only when the model server runs with <code>--enable-force-include-usage</code> (the MaaS docs&apos; model setup).</>,
        <>Use the model in the dashboard&apos;s playground as a Models as a Service model.</>,
      ]} />
    </TabSection>
  );
};

/**
 * The MaaS tab: the platform's documented parts with their health, Set up
 * (in the docs' order), this tool's models with Publish and Unpublish, how
 * to call a published model, what setup creates, and Remove.
 */
export const MaaSTab: React.FC<MaaSTabProps> = (props) => {
  const { maas, maasError, onRetry, state, baseReason, readOnly, setPending, onSetUp } = props;
  const next = state.next;
  const published = (maas?.items ?? []).some((i) => i.published);
  const attention = (maas?.parts ?? []).filter((p) => ["manual", "waiting", "problem"].includes(p.state));
  // Set up whenever it has something to do: also to finish a run that stopped, or fix a part it can fix.
  const setupPlan = maas?.setupPlan ?? [];
  const setupLabel = maas?.platform.managedByTool || maas?.platform.deployed ? "Finish setting up MaaS" : "Set up MaaS";
  const partReason = readOnly ? READ_ONLY_SETUP : baseReason;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="MaaS" purpose={FEATURE_PURPOSE.maas} state={state} /></StackItem>
      {maas?.blocked && <StackItem><ComponentAlert feature="Models as a Service" block={maas.blocked} /></StackItem>}
      {maas && maasError && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="Models as a Service may be out of date" actionLinks={<AlertActionLink onClick={onRetry}>Try again</AlertActionLink>}>
            The parts and models below are from the last successful read. {maasError}
          </Alert>
        </StackItem>
      )}
      <StackItem>
        {maas ? (
          <PartList aria-label="What MaaS needs">
            {maas.parts.map((p) => <PlatformPart key={p.id} part={p} readOnly={readOnly} reason={partReason} setPending={setPending} />)}
          </PartList>
        ) : maasError ? <LoadFailed title="Cannot read Models as a Service" error={maasError} onRetry={onRetry} />
          : <PartsSkeleton aria-label="What MaaS needs" what="what MaaS needs" count={3} />}
      </StackItem>
      {maas && setupPlan.length > 0 && attention.length > 0 && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="Set up runs; these parts need attention too">
            {attention.map((p) => p.title).join(", ")}: each shows above what it needs. MaaS works once they are ready.
          </Alert>
        </StackItem>
      )}
      {maas && (setupPlan.length > 0 || !!maas.database?.needed || next?.kind === "set-up") && (
        <StackItem>
          <PrimaryAction label={next?.kind === "set-up" ? next.label : setupLabel} onClick={() => onSetUp([{ kind: "setup-maas" }])}
            reason={readOnly ? READ_ONLY_SETUP : baseReason ?? maas.platform.setupBlockedReason ?? null} />
        </StackItem>
      )}
      {(maas || !maasError) && <StackItem><PublishedModels {...props} /></StackItem>}
      {maas && state.status === "ready" && published && <StackItem><UseMaaS maas={maas} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>Only what is missing, as the MaaS docs install it: Red Hat Connectivity Link from <code>redhat-operators</code> in <code>kuadrant-system</code>, set to deny traffic when rate limiting fails (the docs&apos; recommendation), and its Kuadrant instance; and Authorino TLS (the docs&apos; <code>setup-authorino-tls.sh</code>). These stay after Clean up: other RHOAI features use them</>,
            <>The AI gateway&apos;s namespace (<code>{maas?.platform.namespace ?? "redhat-ai-gateway-infra"}</code>) when it does not exist yet; Remove deletes it when nothing else is in it</>,
            <>MaaS&apos;s PostgreSQL, as you choose in Set up (the MaaS docs leave it to you): yours (only Secret <code>maas-db-config</code> is written), the docs&apos; development database (one pod, no volume: keys are lost when it restarts), or a persistent one on CloudNativePG, this tool&apos;s own option, which installs the CloudNativePG operator from <code>certified-operators</code> in <code>cnpg-system</code> (it stays after Clean up). A <code>maas-db-config</code> someone else wrote is used as it is</>,
            <>Gateway <code>maas-default-gateway</code> in <code>openshift-ingress</code> with an external load balancer at <code>maas.&lt;apps domain&gt;</code> (and GatewayClass <code>openshift-default</code> when the cluster has none; it stays after Clean up, as OpenShift&apos;s shared class)</>,
            <>MaaS turned on in the DataScienceCluster (<code>aigateway</code> and <code>aigateway.modelsAsAService</code>); RHOAI&apos;s operator then runs maas-api and the default tenant</>,
            <>Publishing: the model routes through the MaaS gateway only, and a MaaSModelRef, the subscription <code>{maas?.subscription ?? "rhoai-nightly-updater-subscription"}</code> and the authorization policy <code>rhoai-nightly-updater-access</code> list it. The subscription&apos;s priority is below every other subscription&apos;s, so it never takes a key meant for someone else&apos;s</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && maas?.platform.managedByTool && (
        <StackItem>
          <CleanupSection id="maas-cleanup" label="Clean up MaaS..." onClick={() => setPending({ kind: "teardown-maas" })} reason={baseReason}
            text="Unpublishes this tool's models, puts the DataScienceCluster back as it was before, then deletes the gateway, the database and the namespace this tool created. A PostgreSQL of your own is never touched. Connectivity Link, Kuadrant, Authorino TLS and CloudNativePG stay, also if this tool installed them: other RHOAI features use them." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The request a confirmed MaaS change sends (POST /api/resources/maas/stream); the forms' defaults where the user changed nothing. */
export function maasRequestOf(p: MaaSPending, m: MaaSStatus | null): MaaSRequest {
  switch (p.kind) {
    case "setup-maas":
      return { action: "setup", ...(m?.database?.needed ? { database: databaseRequest(p.database ?? defaultDatabaseForm(m.database)) } : {}) };
    case "teardown-maas":
      return { action: "teardown" };
    case "publish-maas":
      return { action: "publish", project: p.project, model: p.model, limit: limitRequest(p.limit ?? defaultLimitForm()),
        ...(firstPublication(m) ? { access: audienceRequest(p.access ?? defaultAudienceForm(m?.access)) } : {}) };
    case "unpublish-maas":
      return { action: "unpublish", project: p.project, model: p.model };
    case "maas-limit":
      return { action: "change-limit", project: p.project, model: p.model, limit: limitRequest(p.limit ?? defaultLimitForm(limitOf(m, p.project, p.model))) };
    case "maas-access":
      return { action: "change-access", access: audienceRequest(p.access ?? defaultAudienceForm(m?.access)) };
    case "maas-database":
      return { action: "change-database", database: databaseRequest(p.database ?? defaultDatabaseForm(m?.database, m?.database?.current)), confirmKeysLost: !!p.confirmKeysLost };
    default:
      return { action: p.action };
  }
}

/** No model of this tool is published yet: Publish sets who can use them. */
const firstPublication = (m: MaaSStatus | null) => !(m?.items ?? []).some((i) => i.published);
const limitOf = (m: MaaSStatus | null, project: string, model: string) => (m?.items ?? []).find((i) => i.namespace === project && i.name === model)?.limit;

/** The steps a MaaS operation streams, in its order (pkg/cluster RunMaaS). */
export function maasSteps(p: MaaSPending): { id: string; title: string }[] {
  switch (p.kind) {
    case "maas-limit":
      return [
        { id: "check", title: "Check the publication and the subscription" },
        { id: "limit", title: `Change the limit of ${p.model}` },
        { id: "ready", title: "Wait until MaaS applies it" },
      ];
    case "maas-access":
      return [
        { id: "check", title: "Check the subscription and the authorization policy" },
        { id: "access", title: "Change who can use the published models" },
        { id: "ready", title: "Wait until MaaS applies it" },
      ];
    case "maas-database":
      return [
        { id: "check", title: "Check the database and who uses MaaS" },
        { id: "cnpg", title: "CloudNativePG operator (if chosen)" },
        { id: "database", title: "The new database and its connection Secret" },
        { id: "cleanup", title: "Delete the database before" },
      ];
    case "setup-maas":
      return [
        { id: "check", title: "Check what the platform needs" },
        { id: "rhcl", title: "Red Hat Connectivity Link" },
        { id: "kuadrant", title: "Connectivity Link instance (Kuadrant)" },
        { id: "cnpg", title: "CloudNativePG operator (if chosen)" },
        { id: "database", title: "Database and its connection Secret" },
        { id: "gateway", title: "MaaS gateway" },
        { id: "authorino-tls", title: "Authorino TLS" },
        { id: "dsc", title: "Turn on MaaS in the DataScienceCluster" },
        { id: "ready", title: "Wait for maas-api and the tenant" },
      ];
    case "maas-action":
      switch (p.action) {
        case "upgrade-rhcl":
          return [
            { id: "check", title: "Check the upgrade path" },
            { id: "upgrade", title: "Upgrade Connectivity Link" },
            { id: "authorino-tls", title: "Authorino TLS" },
            { id: "ready", title: "Check the operator" },
          ];
        case "rhcl-memory":
          return [
            { id: "check", title: "Check the operator's memory" },
            { id: "memory", title: "Raise the memory limit" },
            { id: "ready", title: "Wait for OLM" },
          ];
        case "rhcl-deny-on-failure":
          return [
            { id: "check", title: "Check the Subscription" },
            { id: "env", title: "Deny traffic when rate limiting fails" },
            { id: "ready", title: "Wait for OLM" },
          ];
        case "authorino-tls":
          return [
            { id: "check", title: "Check Authorino" },
            { id: "authorino-tls", title: "Configure Authorino TLS" },
          ];
        default:
          return [
            { id: "check", title: "Find the old tier policies" },
            { id: "delete", title: "Delete them" },
          ];
      }
    case "teardown-maas":
      return [
        { id: "check", title: "Check what this tool set up" },
        { id: "unpublish", title: "Unpublish this tool's models" },
        { id: "dsc", title: "Turn MaaS back to what it was" },
        { id: "gateway", title: "Delete the gateway" },
        { id: "database", title: "Delete the database" },
      ];
    case "publish-maas":
      return [
        { id: "check", title: "Check the platform and the model" },
        { id: "gateway", title: `Let the MaaS gateway admit ${p.project}` },
        { id: "model", title: "Route the model through the MaaS gateway only" },
        { id: "maas", title: "Model reference, subscription and authorization policy" },
        { id: "ready", title: "Wait until MaaS lists the model" },
      ];
    default:
      return [
        { id: "check", title: "Check the publication" },
        { id: "maas", title: "Remove the model from MaaS" },
        { id: "model", title: "Route the model as before" },
        { id: "gateway", title: `Stop admitting ${p.project}` },
      ];
  }
}

/** A model's short display name in MaaS's list (as the Models tab names it), or its resource name. */
function maasModelName(m: MaaSStatus | null, project: string, model: string): string {
  const d = m?.items?.find((i) => i.namespace === project && i.name === model)?.displayName;
  return d ? d.replace(/\s*\([^)]*\)\s*$/, "") : model;
}

/** The confirmation of a MaaS change, or null for another feature's change. */
export function maasConfirm(pending: Pending, m: MaaSStatus | null, setPending?: (p: Pending) => void): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-maas": {
      const db = m?.database;
      const form = pending.database ?? defaultDatabaseForm(db);
      const choice = db?.needed ? db.choices.find((c) => c.id === form.kind) : undefined;
      const problem = db?.needed ? databaseProblem(form, db) : null;
      return {
        title: "Set up Models as a Service?", confirm: "Set up", runTitle: "Set up Models as a Service",
        summary: <Content component="p">Installs whatever the MaaS install docs list that this cluster doesn't have yet, in the docs' order. On a cluster without any of it this takes about 10 minutes. The gateway creates a cloud load balancer, which costs money while it exists. If a step fails, Set up again continues from there.</Content>,
        choice: db?.needed ? <DatabaseChoice db={db} value={form} idPrefix="setup" onChange={(f) => setPending?.({ kind: "setup-maas", database: f })} />
          : db && db.current ? <Content component="p">Database: {databaseLabel(db.current)}{db.current === "adopted" ? ", used as it is" : ""}.</Content> : undefined,
        confirmDisabled: !!problem,
        changes: [
          ...(m?.setupPlan ?? []).map((c) => <>{c}.</>),
          ...(choice?.plan ?? []).map((c) => <>Database: {c}.</>),
          <>Not changed: anything that exists already and was not created by this tool, other than the settings listed above.</>,
        ],
      };
    }
    case "maas-database": {
      const db = m?.database;
      if (!db) return null;
      const form = pending.database ?? defaultDatabaseForm(db, db.current);
      const choice = db.choices.find((c) => c.id === form.kind);
      const problem = databaseProblem(form, db) ?? (form.kind === db.current && form.kind !== "external" ? "Choose another database." : null);
      const before = db.current === "development" ? <>Deployment <code>postgres</code>, Service <code>postgres</code> and Secret <code>postgres-creds</code></>
        : db.current === "cnpg" ? <>CloudNativePG Cluster <code>maas-db</code> with its volume (the operator stays)</> : null;
      const yoursToYours = db.current === "external" && form.kind === "external";
      const kept = "This cannot be undone. Published models, who can use them, and their limits are kept.";
      return {
        title: "Change the MaaS database?", confirm: "Change database", danger: true, runTitle: "Change the MaaS database",
        summary: <Content component="p">
          {before
            ? <>Creates the new database, points maas-api at it and restarts maas-api, then permanently deletes the current one, {createdDatabaseInSentence(db.current)}.</>
            : yoursToYours
              ? <>Points maas-api at the new URL and restarts maas-api. Your current PostgreSQL is not changed or deleted.</>
              : <>Creates the new database, points maas-api at it and restarts maas-api. Your current PostgreSQL is not changed or deleted.</>}
        </Content>,
        choice: <DatabaseChoice db={db} value={form} except={db.current} idPrefix="change" onChange={(f) => setPending?.({ ...pending, database: f })} />,
        // Data is deleted only with a database this tool made; from yours, keys stop working but nothing is deleted.
        dataLoss: before ? <>Every API key, with the current database: {before}. {kept}</> : undefined,
        extra: (
          <Stack hasGutter>
            {!before && form.kind && (
              <StackItem>
                {yoursToYours
                  ? <Alert variant="warning" isInline component="p" title="API keys are lost if this is a different database">Pointing at the same database (for example, with a new password) keeps every key.</Alert>
                  : <Alert variant="warning" isInline component="p" title="Every API key stops working">The new database starts empty. Your PostgreSQL keeps its data, but MaaS no longer uses it. Published models, who can use them, and their limits are kept.</Alert>}
              </StackItem>
            )}
            <StackItem><KeysLostCheck checked={!!pending.confirmKeysLost} onChange={(v) => setPending?.({ ...pending, database: form, confirmKeysLost: v })} /></StackItem>
          </Stack>
        ),
        confirmDisabled: !!problem || !pending.confirmKeysLost,
        changes: [
          ...(choice?.plan ?? []).map((c) => <>{c}.</>),
          <>maas-api restarts to read the new Secret (if it runs).</>,
          before ? <>Deleted: {before}, with every API key in it.</> : <>Your PostgreSQL before is not touched.</>,
        ],
      };
    }
    case "maas-limit": {
      const item = (m?.items ?? []).find((i) => i.namespace === pending.project && i.name === pending.model);
      const form = pending.limit ?? defaultLimitForm(item?.limit);
      return {
        title: `Change the limit of ${maasModelName(m, pending.project, pending.model)} in ${pending.project}?`, confirm: "Change limit", runTitle: `Change the limit of ${maasModelName(m, pending.project, pending.model)} in ${pending.project}`,
        summary: <Content component="p">Now: {limitText(item?.limit)}. API keys keep working; MaaS applies the new limit within seconds.</Content>,
        choice: <LimitFields value={form} idPrefix="change" noUnlimited={m?.noUnlimited} onChange={(f) => setPending?.({ ...pending, limit: f })} />,
        confirmDisabled: !!limitProblem(form, m?.noUnlimited),
        changes: [<>MaaSSubscription <code>{m?.subscription ?? "rhoai-nightly-updater-subscription"}</code> in <code>models-as-a-service</code>: the model&apos;s entry only (after a server-side dry run); the other models and who can use them stay.</>],
      };
    }
    case "maas-access": {
      const form = pending.access ?? defaultAudienceForm(m?.access);
      return {
        title: "Change who can use the published models?", confirm: "Change access", runTitle: "Change who can use the published MaaS models",
        summary: <Content component="p">Now: {audienceText(m?.access)}. It applies to every model this tool published; anyone left out can no longer call them, also with a key they made before.</Content>,
        choice: <AudienceFields value={form} idPrefix="change" onChange={(f) => setPending?.({ ...pending, access: f })} />,
        confirmDisabled: !!audienceProblem(form),
        changes: [<>In <code>models-as-a-service</code>: the owners of subscription <code>{m?.subscription ?? "rhoai-nightly-updater-subscription"}</code> and the subjects of authorization policy <code>rhoai-nightly-updater-access</code> (after a server-side dry run). The limits stay.</>],
      };
    }
    case "maas-action": {
      const action = m?.parts.flatMap((part) => part.actions ?? []).find((a) => a.id === pending.action);
      if (!action) return null;
      return {
        title: `${action.label}?`, confirm: action.label, danger: action.danger, runTitle: action.label,
        summary: <Content component="p">{action.summary}</Content>,
        changes: action.changes.map((c) => <>{c}</>),
      };
    }
    case "teardown-maas":
      return {
        title: "Clean up Models as a Service?", confirm: "Clean up", danger: true, runTitle: "Clean up Models as a Service",
        dataLoss: <>Every API key, with {createdDatabaseInSentence(m?.database?.current ?? "")} and its data. A PostgreSQL of your own is never touched.</>,
        summary: <Content component="p">Removes what this tool set up, in reverse order. API keys stop working, and the database&apos;s data (the keys&apos; records) is deleted. It refuses, before changing anything, while others use MaaS: model references, subscriptions or authorization policies this tool did not create. It keeps the gateway while routes are attached to it.</Content>,
        changes: [
          <>Unpublished: this tool&apos;s models ({(m?.items ?? []).filter((i) => i.published).map((i) => `${i.name} in ${i.namespace}`).join(", ") || "none"}).</>,
          <>DataScienceCluster: <code>aigateway.modelsAsAService</code> and <code>aigateway</code> back to their values before setup (each only if this tool changed it and nobody has set it since).</>,
          <>Deleted: Gateway <code>maas-default-gateway</code>; {createdDatabaseInSentence(m?.database?.current ?? "")} with its objects and data, and Secret <code>maas-db-config</code> if this tool wrote it. A PostgreSQL of your own and a Secret someone else wrote are never touched; namespace <code>{m?.platform.namespace ?? "redhat-ai-gateway-infra"}</code> only if this tool created it and nothing else is in it.</>,
          <>Kept, also if this tool installed or set them up: Red Hat Connectivity Link, its Kuadrant instance and Authorino TLS, and the CloudNativePG operator. They are shared platform pieces: RHOAI&apos;s inference gateway authentication uses Kuadrant. Remove them from the OpenShift console if nothing needs them.</>,
          <>Kept: GatewayClass <code>openshift-default</code>, also if this tool created it: it is OpenShift&apos;s shared class for gateways and costs nothing on its own. Also kept: the namespaces RHOAI made, and every object that was not created by this tool.</>,
        ],
      };
    case "publish-maas": {
      const first = firstPublication(m);
      const limit = pending.limit ?? defaultLimitForm();
      const access = pending.access ?? defaultAudienceForm(m?.access);
      return {
        title: `Publish ${maasModelName(m, pending.project, pending.model)} in ${pending.project} to MaaS?`, confirm: "Publish", runTitle: `Publish ${maasModelName(m, pending.project, pending.model)} in ${pending.project}`,
        summary: <Content component="p">Who you choose can then call the model through MaaS with an API key, each up to the limit you choose. While it is published, the model is used through MaaS and the dashboard&apos;s playground; its direct address (the Models tab&apos;s commands) answers 404 until it is unpublished.</Content>,
        choice: (
          <Stack hasGutter>
            {first ? <AudienceFields value={access} idPrefix="publish" onChange={(f) => setPending?.({ ...pending, access: f, limit })} />
              : <Content component="p">Who can use it: {audienceText(m?.access)}, as for the other published models (Change who can use them changes it for all).</Content>}
            <LimitFields value={limit} idPrefix="publish" noUnlimited={m?.noUnlimited} onChange={(f) => setPending?.({ ...pending, limit: f, access: first ? access : undefined })} />
          </Stack>
        ),
        confirmDisabled: !!limitProblem(limit, m?.noUnlimited) || (first && !!audienceProblem(access)),
        changes: [
          <>Gateway <code>maas-default-gateway</code>: admits routes from {pending.project}.</>,
          <>LLMInferenceService <code>{pending.model}</code> in {pending.project}: routes through the MaaS gateway only (only its gateways are changed). The gateways it uses now are recorded, and Unpublish puts them back.</>,
          <>Created: MaaSModelRef <code>{pending.model}</code> in {pending.project}; in <code>models-as-a-service</code>, subscription <code>{m?.subscription ?? "rhoai-nightly-updater-subscription"}</code> (with the model&apos;s limit) and authorization policy <code>rhoai-nightly-updater-access</code> list the model.</>,
        ],
      };
    }
    case "unpublish-maas": {
      const maasOnly = !!m?.items?.find((i) => i.namespace === pending.project && i.name === pending.model)?.maasOnlySetup;
      return {
        title: `Unpublish ${maasModelName(m, pending.project, pending.model)} in ${pending.project}?`, confirm: "Unpublish", danger: true, runTitle: `Unpublish ${maasModelName(m, pending.project, pending.model)} in ${pending.project}`,
        summary: <Content component="p">MaaS stops offering the model; API keys can no longer call it. The model keeps running in {pending.project}.{maasOnly ? " It had no outside route before it was published, so afterwards it has no outside address: only programs inside the cluster can call it, without a token." : ""}</Content>,
        changes: [
          <>Deleted: MaaSModelRef <code>{pending.model}</code> in {pending.project}, and the model&apos;s entries in the subscription and authorization policy (both are deleted when no model is left).</>,
          maasOnly
            ? <>The model had no route of its own before it was published, so its route is removed: it then answers only at its in-cluster address, as a model set up &quot;Only inside the cluster&quot;. The MaaS gateway stops admitting {pending.project} unless another model of this tool is published there; the inference gateway is not changed.</>
            : <>The model routes through the gateways it used before publishing, so its direct address answers again; the MaaS gateway stops admitting {pending.project} unless another model of this tool is published there.</>,
        ],
      };
    }
    default:
      return null;
  }
}
