import React, { useCallback, useEffect, useState } from "react";
import {
  ActionListItem,
  Alert,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { MaaSModel, MaaSPart, MaaSStatus } from "../../types";
import { getMaaS, toApiError } from "../../services/api";
import { usePolling } from "../../hooks/usePolling";
import { RESOURCE_SETTLE_MAX_MS } from "../../constants";
import { type ConfirmSpec, type MaaSPending, type Pending, ExternalLink } from "./resourceRow";
import { CleanupSection, CreatesList, FeatureHeader, FeatureSection, PartItem, PartList, PrimaryAction, TabSection, WhatNext } from "./FeatureLayout";
import { ComponentAlert, CopyableCode } from "./UseIt";
import { EnvStatusLabel } from "./resourceStatus";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { type Busy, type FeatureState, maasDashboardHref, maasPartStatus } from "./featureState";

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
  maas: MaaSStatus | null;
  maasError: string | null;
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

/** The key-mint, model list and chat requests of the MaaS docs (validation.md), with the user's own token. */
export function maasCurl(url: string, subscription: string): string {
  return [
    `HOST="${url}"`,
    "# 1. A MaaS API key for the subscription (shown once: keep it)",
    `API_KEY=$(curl -sS -X POST "$HOST/maas-api/v1/api-keys" \\`,
    `  -H "Authorization: Bearer $(oc whoami -t)" -H "Content-Type: application/json" \\`,
    `  -d '{"name":"my-key","expiresIn":"1h","subscription":"${subscription}"}' | jq -r .key)`,
    "# 2. The models the key may use",
    `MODEL=$(curl -sS "$HOST/maas-api/v1/models" -H "Authorization: Bearer $API_KEY" | jq -r '[.data[] | select(.ready==true)][0].id')`,
    "# 3. Chat",
    `curl -sS "$HOST/v1/chat/completions" -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \\`,
    `  -d "{\\"model\\": \\"$MODEL\\", \\"messages\\": [{\\"role\\": \\"user\\", \\"content\\": \\"Hello\\"}], \\"max_tokens\\": 50}"`,
  ].join("\n");
}

/** One platform part. */
const PlatformPart: React.FC<{ part: MaaSPart }> = ({ part }) => (
  <PartItem id={`maas-${part.id}`} title={part.title} scope="Shared by the cluster" status={maasPartStatus(part)} unmanaged={!!part.notManaged}
    actions={part.docUrl ? <ActionListItem><ExternalLink href={part.docUrl}>Docs</ExternalLink></ActionListItem> : undefined}>
    {part.message && <Content component="p">{part.message}</Content>}
    {part.warning && <Alert variant="warning" isInline component="p" title="Check the version">{part.warning}</Alert>}
    {part.commands && <CopyableCode id={`maas-${part.id}-commands`} label="the commands for a cluster-admin" code={part.commands} />}
  </PartItem>
);

/** The words for a model's publication. */
export function maasPublication(i: MaaSModel): string {
  if (!i.published) return "Not published";
  if (i.partly) return i.message ?? "Partly published. Unpublish to finish removing it.";
  if (i.ready) return "Published";
  return `Published, waiting (reference ${i.phase || "pending"}, access ${i.access || "pending"}, subscription ${i.subscription || "pending"})`;
}

/** This tool's models and their publication: Publish / Unpublish per model. */
const PublishedModels: React.FC<MaaSTabProps> = ({ maas, busy, baseReason, readOnly, setPending, onModels }) => {
  const items = maas?.items ?? [];
  const running = (project: string, model?: string) => busy.maas?.project === project && (!busy.maas?.model || busy.maas.model === model);
  return (
    <TabSection title="Published models" id="maas-models">
      <Content component="p">
        Publishing offers one of this tool&apos;s chat models to everyone signed in to the cluster through MaaS, with a limit of 100 tokens a minute each (the docs&apos; sample; counted from the token usage a response reports). While it is published, the model is used through MaaS (with an API key) and the dashboard&apos;s playground; its direct address on the Models tab answers 404 until it is unpublished. Unpublish routes it as before.
      </Content>
      {items.length === 0 ? (
        <Content component="p">
          No model of this tool yet. <Button variant="link" isInline onClick={onModels}>Set one up on the Models tab</Button>, then publish it here.
        </Content>
      ) : (
        <Table variant="compact" aria-label="This tool's models in MaaS">
          <Thead>
            <Tr>
              <Th modifier="wrap">Model</Th>
              <Th modifier="wrap">Project</Th>
              <Th modifier="wrap">In MaaS</Th>
              <Th screenReaderText="Actions" />
            </Tr>
          </Thead>
          <Tbody>
            {items.map((i) => {
              const project = i.namespace!;
              const model = i.name!;
              const reason = readOnly ? READ_ONLY_SETUP : baseReason ?? i.setupBlockedReason ?? null;
              return (
                <Tr key={`${project}/${model}`}>
                  <Td dataLabel="Model" modifier="wrap">{i.displayName || i.name}</Td>
                  <Td dataLabel="Project" modifier="wrap">{project}</Td>
                  <Td dataLabel="In MaaS" modifier="wrap">
                    <Stack>
                      <StackItem><EnvStatusLabel status={running(project, model) ? (busy.maas?.action === "unpublish" ? "deleting" : "setting-up") : i.partly ? "needs-attention" : i.published ? (i.ready ? "ready" : "setting-up") : "not-set-up"} /></StackItem>
                      <StackItem><Content component="small">{maasPublication(i)}</Content></StackItem>
                    </Stack>
                  </Td>
                  <Td dataLabel="Actions" isActionCell>
                    {!readOnly && (i.published
                      ? <Button variant="secondary" isDanger size="sm" isAriaDisabled={!!baseReason} onClick={() => setPending({ kind: "unpublish-maas", project, model })}>Unpublish</Button>
                      : <Button variant="secondary" size="sm" isAriaDisabled={!!reason} onClick={() => setPending({ kind: "publish-maas", project, model })}>Publish</Button>)}
                    {!readOnly && !i.published && reason && <Content component="small">{reason}</Content>}
                  </Td>
                </Tr>
              );
            })}
          </Tbody>
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
          <DescriptionListDescription><code>{maas.url}</code></DescriptionListDescription>
        </DescriptionListGroup>
        <DescriptionListGroup>
          <DescriptionListTerm>Subscription</DescriptionListTerm>
          <DescriptionListDescription><code>{maas.subscription}</code> <Label isCompact>100 tokens a minute</Label></DescriptionListDescription>
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
            <DescriptionListDescription><ExternalLink href={href}>Models as a Service</ExternalLink>: API keys, subscriptions and models</DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
      <WhatNext id="maas-next" items={[
        <>Run the commands: the chat answers, and a request without a key gets 401. More than 100 tokens a minute gets 429 for calls whose response reports its token usage, as the non-streaming chat above does; streaming responses report it only when the model server runs with <code>--enable-force-include-usage</code> (the MaaS docs&apos; model setup).</>,
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
  const { maas, maasError, state, baseReason, readOnly, setPending, onSetUp } = props;
  const next = state.next;
  const published = (maas?.items ?? []).some((i) => i.published);
  const attention = (maas?.parts ?? []).filter((p) => ["manual", "waiting", "problem"].includes(p.state));
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="MaaS" purpose="Models as a Service: offer models to everyone on the cluster through one gateway, with API keys and token limits." state={state} /></StackItem>
      {maas?.blocked && <StackItem><ComponentAlert feature="Models as a Service" block={maas.blocked} /></StackItem>}
      {maasError && <StackItem><Alert variant="danger" isInline component="p" title="Cannot read Models as a Service">{maasError}</Alert></StackItem>}
      <StackItem>
        <PartList aria-label="What MaaS needs">
          {(maas?.parts ?? []).map((p) => <PlatformPart key={p.id} part={p} />)}
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && attention.length > 0 && (
        <StackItem>
          <Alert variant="warning" isInline component="p" title="Set up runs; these parts are not ready yet">
            {attention.map((p) => p.title).join(", ")}: each shows above what it needs. MaaS works once they are ready.
          </Alert>
        </StackItem>
      )}
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason} />
        </StackItem>
      )}
      <StackItem><PublishedModels {...props} /></StackItem>
      {maas && state.status === "ready" && published && <StackItem><UseMaaS maas={maas} /></StackItem>}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>A PostgreSQL database for MaaS&apos;s API keys: one CloudNativePG instance with a 1 GiB volume, for development and test, and Secret <code>maas-db-config</code> with its address, in the AI gateway&apos;s namespace</>,
            <>Gateway <code>maas-default-gateway</code> in <code>openshift-ingress</code> with an external load balancer at <code>maas.&lt;apps domain&gt;</code> (and GatewayClass <code>openshift-default</code> when the cluster has none; it stays after Remove, as OpenShift&apos;s shared class)</>,
            <>MaaS turned on in the DataScienceCluster (<code>aigateway.modelsAsAService</code>); RHOAI&apos;s operator then runs maas-api and the default tenant</>,
            <>Publishing: the model routes through the MaaS gateway only, and a MaaSModelRef, the subscription <code>{maas?.subscription ?? "rhoai-nightly-updater-subscription"}</code> and the access policy <code>rhoai-nightly-updater-access</code> list it</>,
            <>Not done by this tool: Connectivity Link (Kuadrant) and its Authorino TLS, which a cluster-admin sets up (the parts above show how)</>,
          ]} />
        </FeatureSection>
      </StackItem>
      {!readOnly && maas?.platform.managedByTool && (
        <StackItem>
          <CleanupSection id="maas-cleanup" label="Remove MaaS..." onClick={() => setPending({ kind: "teardown-maas" })} reason={baseReason}
            text="Unpublishes this tool's models, turns MaaS back off as it was before, then deletes the gateway and the database this tool created. Connectivity Link and what RHOAI's operator made stay." />
        </StackItem>
      )}
    </Stack>
  );
};

/** The steps a MaaS operation streams, in its order (pkg/cluster RunMaaS). */
export function maasSteps(p: MaaSPending): { id: string; title: string }[] {
  switch (p.kind) {
    case "setup-maas":
      return [
        { id: "check", title: "Check what the platform needs" },
        { id: "database", title: "Database and its connection Secret" },
        { id: "gateway", title: "Gateway maas-default-gateway" },
        { id: "authorino-tls", title: "Check Authorino TLS" },
        { id: "dsc", title: "Turn on MaaS in the DataScienceCluster" },
        { id: "ready", title: "Wait for maas-api and the tenant" },
      ];
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
        { id: "model", title: `Route ${p.model} through the MaaS gateway only` },
        { id: "maas", title: "Model reference, subscription and access policy" },
        { id: "ready", title: "Wait until MaaS lists the model" },
      ];
    default:
      return [
        { id: "check", title: "Check the publication" },
        { id: "maas", title: `Remove ${p.model} from MaaS` },
        { id: "model", title: `Route ${p.model} as before` },
        { id: "gateway", title: `Stop admitting ${p.project}` },
      ];
  }
}

/** The confirmation of a MaaS change, or null for another feature's change. */
export function maasConfirm(pending: Pending, m: MaaSStatus | null): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-maas":
      return {
        title: "Set up Models as a Service?", confirm: "Set up", runTitle: "Set up Models as a Service",
        summary: <Content component="p">Sets up the MaaS platform in the order the MaaS install docs give. It takes about 5 minutes. The database is a small one for development and test. The gateway creates a cloud load balancer, which costs money while it exists.</Content>,
        changes: [
          <>Namespace <code>{m?.platform.namespace ?? "the AI gateway's"}</code>: a one-instance CloudNativePG database <code>maas-db</code> (1 GiB volume) and Secret <code>maas-db-config</code> with its address.</>,
          <>Namespace <code>openshift-ingress</code>: Gateway <code>maas-default-gateway</code> with an external load balancer (cloud cost) at <code>{m?.url ?? "maas.<apps domain>"}</code>; GatewayClass <code>openshift-default</code> only if the cluster has none.</>,
          <>DataScienceCluster: <code>aigateway.modelsAsAService</code> set to Managed; its current value is recorded, and Remove puts it back.</>,
          <>Not changed: Connectivity Link (Kuadrant) and Authorino, and anything that exists already and was not created by this tool.</>,
        ],
      };
    case "teardown-maas":
      return {
        title: "Remove Models as a Service?", confirm: "Remove", danger: true, runTitle: "Remove Models as a Service",
        summary: <Content component="p">Removes what this tool set up, in reverse order. API keys stop working, and the database&apos;s data (the keys&apos; records) is deleted. It refuses, before changing anything, while others use MaaS: model references, subscriptions or access policies this tool did not create. It keeps the gateway while routes are attached to it.</Content>,
        changes: [
          <>Unpublished: this tool&apos;s models ({(m?.items ?? []).filter((i) => i.published).map((i) => `${i.name} in ${i.namespace}`).join(", ") || "none"}).</>,
          <>DataScienceCluster: <code>aigateway.modelsAsAService</code> back to its value before setup (only if this tool changed it and nobody has set it since).</>,
          <>Deleted: Gateway <code>maas-default-gateway</code>, the database <code>maas-db</code> with its volume, and Secret <code>maas-db-config</code>.</>,
          <>Kept: GatewayClass <code>openshift-default</code>, also if this tool created it: it is OpenShift&apos;s shared class for gateways and costs nothing on its own. Also kept: Connectivity Link, Authorino, the CloudNativePG operator, the namespaces RHOAI made, and every object that was not created by this tool.</>,
        ],
      };
    case "publish-maas":
      return {
        title: `Publish ${pending.model} in ${pending.project} to MaaS?`, confirm: "Publish", runTitle: `Publish ${pending.model} in ${pending.project}`,
        summary: <Content component="p">Everyone signed in to the cluster can then call the model through MaaS with an API key, up to 100 tokens a minute each. While it is published, the model is used through MaaS and the dashboard's playground; its direct address (the Models tab's commands) answers 404 until it is unpublished.</Content>,
        changes: [
          <>Gateway <code>maas-default-gateway</code>: admits routes from {pending.project}.</>,
          <>LLMInferenceService <code>{pending.model}</code> in {pending.project}: routes through the MaaS gateway only (only its gateways are changed). The gateways it uses now are recorded, and Unpublish puts them back.</>,
          <>Created: MaaSModelRef <code>{pending.model}</code> in {pending.project}; in <code>models-as-a-service</code>, subscription <code>{m?.subscription ?? "rhoai-nightly-updater-subscription"}</code> and access policy <code>rhoai-nightly-updater-access</code> list the model.</>,
        ],
      };
    case "unpublish-maas":
      return {
        title: `Unpublish ${pending.model} in ${pending.project}?`, confirm: "Unpublish", danger: true, runTitle: `Unpublish ${pending.model} in ${pending.project}`,
        summary: <Content component="p">MaaS stops offering the model; API keys can no longer call it. The model keeps running in {pending.project}.</Content>,
        changes: [
          <>Deleted: MaaSModelRef <code>{pending.model}</code> in {pending.project}, and the model&apos;s entries in the subscription and access policy (both are deleted when no model is left).</>,
          <>The model routes through the gateways it used before publishing, so its direct address answers again; the MaaS gateway stops admitting {pending.project} unless another model of this tool is published there.</>,
        ],
      };
    default:
      return null;
  }
}
