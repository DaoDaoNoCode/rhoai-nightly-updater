import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Checkbox,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { ModelRegistryStatus } from "../../types";
import { ImageRef } from "../ImageRef";
import { TruncatedText } from "../LongText";
import { TooltipButton } from "../TooltipButton";
import { type ConfirmSpec, type ModelRegistryPending, type Pending, ExternalLink, WarningAlert } from "./resourceRow";
import { CleanupSection, CreatesList, FEATURE_PURPOSE, FeatureHeader, FeatureSection, PartItem, PartList, type PartMenuItem, PrimaryAction, READ_ONLY_SETUP, WhatNext } from "./FeatureLayout";
import { DEFAULT_MODEL, type Busy, type FeatureState, modelRegistryDashboardHref, modelRegistrySampleStatus, modelRegistryStatus } from "./featureState";
import { LoadFailed } from "./Loading";

/** Documentation for model registries in the current RHOAI release. */
export const MODEL_REGISTRY_DOCS = "https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/latest/html-single/managing_model_registries/index";

/** Name of the registry provisioned by this tool (pkg/cluster/internal/modelregistry Name). */
export const MODEL_REGISTRY_NAME = "nightly-registry";

/** The link to the registry's page in the dashboard (the tab and the Overview). */
export const OPEN_MODEL_REGISTRY = "Open model registry";

/** The sample, named the same way everywhere (pkg/cluster/internal/modelregistry: the Models tab's default model). */
export const SampleName: React.FC = () => <>Sample model <code>{DEFAULT_MODEL}</code> (the Models tab&apos;s default chat model), version v1</>;

/** A message as a sentence: with its final period. */
const sentence = (s: string) => (/[.!?]$/.test(s.trim()) ? s.trim() : `${s.trim()}.`);

export interface ModelRegistryTabProps {
  /** Cluster prerequisites for one-click setup, rendered above the part list. */
  prerequisites?: React.ReactNode;
  /** Explains blocked setup when prerequisites are missing; null when setup may proceed. */
  prerequisitesReason?: string | null;
  registry: ModelRegistryStatus | null;
  registryError: string | null;
  onRetry: () => void;
  /** modelRegistryState mirrors the Overview tab's feature state. */
  state: FeatureState;
  busy: Busy;
  baseReason: string | null;
  readOnly: boolean;
  setPending: (p: Pending) => void;
  onSetUp: (setup: Pending[]) => void;
  onCleanup: () => void;
}

const where = (mr: ModelRegistryStatus | null) => (mr?.namespace ? <code>{mr.namespace}</code> : "the namespace RHOAI keeps model registries in");

/**
 * Model registry tab UI: prerequisites (DataScienceCluster), the tool-managed
 * registry using the dashboard default database, sample model, created
 * resources, and cleanup.
 */
export const ModelRegistryTab: React.FC<ModelRegistryTabProps> = (props) => {
  const { registry: mr, registryError, state, busy, baseReason, readOnly, setPending, onSetUp, onCleanup } = props;
  const status = modelRegistryStatus(mr, busy);
  const foreign = !!mr?.foreign;
  const ours = !!mr?.managedByTool;
  const present = !!mr && mr.state !== "absent";
  const href = modelRegistryDashboardHref(mr);
  const next = state.next;
  const actions: React.ReactNode[] = [];
  if (mr?.state === "ready" && href) actions.push(<ActionListItem key="open"><ExternalLink href={href}>{OPEN_MODEL_REGISTRY}</ExternalLink></ActionListItem>);
  const menu: PartMenuItem[] = ours && !readOnly && mr?.state !== "terminating"
    ? [{ key: "delete", label: "Delete the model registry...", onClick: () => setPending({ kind: "teardown-model-registry" }), disabledReason: baseReason, isDanger: true }]
    : [];
  const sampleStatus = modelRegistrySampleStatus(mr);
  const sample = mr?.sample;
  const canAddSample = ours && mr?.state === "ready" && !!mr.address && !readOnly && sampleStatus !== "ready" && !sample?.problem;
  const archived = !!sample?.registered && sample.state !== "LIVE";
  const incomplete = !!sample?.registered && !archived && !sample.complete;
  return (
    <Stack hasGutter>
      <StackItem><FeatureHeader title="Model registry" purpose={FEATURE_PURPOSE["model-registry"]} state={state} /></StackItem>
      {props.prerequisites && <StackItem>{props.prerequisites}</StackItem>}
      {!mr && registryError && <StackItem><LoadFailed title="Cannot read the model registry" error={registryError} onRetry={props.onRetry} /></StackItem>}
      {mr && registryError && (
        <StackItem>
          <Alert variant="warning" title="Model registry status may be out of date" isInline component="p"
            actionLinks={<AlertActionLink onClick={props.onRetry}>Retry</AlertActionLink>}>
            <TruncatedText>The data below is from the last successful refresh. {registryError}</TruncatedText>
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <PartList aria-label="What the model registry needs">
          <PartItem id="model-registry" title="Model registry" scope="Shared by the cluster" status={status} unmanaged={foreign} actions={actions.length ? actions : undefined} menu={menu} loading={!mr && !registryError}>
            <Content component="p">
              One model registry, {MODEL_REGISTRY_NAME}, in {where(mr)}, with the dashboard&apos;s default database (PostgreSQL, for development and testing).
              {foreign ? " It was created outside this tool (for example in the dashboard): this tool shows it, but never changes or deletes it." : ""}
            </Content>
            {mr?.message && mr.state !== "ready" && <Content component="p">{sentence(mr.message)}</Content>}
            {mr?.state === "ready" && mr.message && <WarningAlert text={sentence(mr.message)} />}
            {mr?.addressProblem && <WarningAlert text={`The registry's address is not used, so no sample can be added: ${sentence(mr.addressProblem)}`} />}
            {present && (
              <DescriptionList isCompact isHorizontal aria-label="Model registry details">
                <DescriptionListGroup>
                  <DescriptionListTerm>Name</DescriptionListTerm>
                  <DescriptionListDescription><code>{mr?.namespace}/{mr?.name}</code></DescriptionListDescription>
                </DescriptionListGroup>
                {mr?.address && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>REST address</DescriptionListTerm>
                    <DescriptionListDescription><ImageRef image={mr.address} what="registry address" truncate={false} /></DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                {mr?.component && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>Run by</DescriptionListTerm>
                    <DescriptionListDescription>Component <code>{mr.component}</code> of the DataScienceCluster</DescriptionListDescription>
                  </DescriptionListGroup>
                )}
              </DescriptionList>
            )}
          </PartItem>
          <PartItem id="model-registry-sample" title="Sample model" scope={`In ${MODEL_REGISTRY_NAME}`} status={sampleStatus} unmanaged={false}
            statusText={sampleStatus === "not-set-up" ? "Not added" : archived ? "Archived" : incomplete ? "Incomplete" : undefined} loading={!mr && !registryError}
            actions={canAddSample ? <ActionListItem><TooltipButton variant="secondary" disabledReason={baseReason}
              onClick={() => setPending({ kind: "setup-model-registry", samples: true })}>{archived ? "Restore the sample" : "Add the sample"}</TooltipButton></ActionListItem> : undefined}>
            <Content component="p">
              <SampleName />, with its weights {sample?.uri ? <>at <code>{sample.uri}</code></> : "on Hugging Face"}.
              {incomplete ? " Its version or its model artifact is missing or not live: Add the sample completes it." : ""}
              {!ours || mr?.state !== "ready" ? " It can be added once this tool's registry is ready." : !mr.address ? " This registry has no address through the cluster's gateway, so it cannot be added." : ""}
            </Content>
            {sample?.problem && <WarningAlert text={sample.problem} />}
          </PartItem>
        </PartList>
      </StackItem>
      {next?.kind === "set-up" && (
        <StackItem>
          <PrimaryAction label={next.label} variant={props.prerequisitesReason ? "secondary" : "primary"} onClick={() => onSetUp(next.setup)} reason={readOnly ? READ_ONLY_SETUP : baseReason ?? next.reason ?? props.prerequisitesReason ?? null} />
        </StackItem>
      )}
      {mr?.state === "ready" && href && (
        <StackItem>
          <WhatNext id="model-registry-next" items={[
            <>Open the registry in the dashboard (AI hub, Registry) to see {sample?.registered ? <>the sample model <code>{sample.model}</code></> : "its models"}.</>,
            <>Register your own model there, from a URI or from object storage.</>,
            <>Deploy a registered version from the dashboard; the deployment links back to the registry.</>,
          ]} />
        </StackItem>
      )}
      <StackItem>
        <FeatureSection title="What this creates">
          <CreatesList items={[
            <>DataScienceCluster: the component that runs model registries is set to Managed, if it is not (its prerequisites, above).</>,
            <>ModelRegistry <code>{MODEL_REGISTRY_NAME}</code> in {where(mr)}, exactly as the dashboard&apos;s Create model registry dialog makes it with the default database. Its operator creates the registry server, a PostgreSQL database with a 5 GiB volume, a Service and a route through the cluster&apos;s gateway.</>,
            <>With the sample: <SampleName /> and its model artifact, in the registry&apos;s database.</>,
            <>No access is granted to anyone: cluster-admins can use the registry; others are added on the dashboard&apos;s Manage permissions page.</>,
          ]} />
          <Content component="p"><ExternalLink href={MODEL_REGISTRY_DOCS} isInline>RHOAI docs: Managing model registries</ExternalLink></Content>
        </FeatureSection>
      </StackItem>
      {!readOnly && ours && mr?.state !== "terminating" && (
        <StackItem>
          <CleanupSection id="model-registry-cleanup" label="Clean up the model registry..." onClick={onCleanup} reason={baseReason}
            text="Deletes the model registry this tool created, with every registered model in it and its database." />
        </StackItem>
      )}
    </Stack>
  );
};

/** Progress step titles streamed during a model registry run (pkg/cluster/internal/modelregistry Run). */
export function modelRegistrySteps(p: ModelRegistryPending): { id: string; title: string }[] {
  return p.kind === "setup-model-registry"
    ? [
      { id: "check", title: "Check that the cluster can run a model registry" },
      { id: "create", title: `Create model registry ${MODEL_REGISTRY_NAME}` },
      { id: "ready", title: "Wait until it is available" },
      ...(p.samples ? [{ id: "samples", title: "Register the sample model" }] : []),
    ]
    : [
      { id: "check", title: "Check the model registry" },
      { id: "delete", title: `Delete model registry ${MODEL_REGISTRY_NAME}` },
      { id: "gone", title: "Wait until it is gone" },
    ];
}

/** Builds the confirm dialog for a model registry action; null when pending is unrelated. */
export function modelRegistryConfirm(pending: Pending, mr: ModelRegistryStatus | null, setPending: (p: Pending) => void): ConfirmSpec | null {
  switch (pending.kind) {
    case "setup-model-registry": {
      const exists = !!mr?.managedByTool && mr.state !== "absent";
      const samples = !!pending.samples;
      return {
        title: exists ? "Add the sample model?" : "Set up the model registry?", confirm: exists ? "Add" : "Set up",
        runTitle: exists ? "Add the sample model" : "Set up the model registry",
        summary: <Content component="p">{exists
          ? <>Adds the <SampleName /> to {MODEL_REGISTRY_NAME}, or completes it.</>
          : <>One model registry for the cluster, as the dashboard creates it with its default database (for development and testing, not production). Takes 1 to 3 minutes.</>}</Content>,
        choice: exists ? undefined : (
          <Checkbox id="model-registry-samples" label="Add a sample model" isChecked={samples} onChange={(_e, v) => setPending({ kind: "setup-model-registry", samples: v })}
            description={<><SampleName />. Registered models cannot be deleted, only archived; they are deleted when the registry is cleaned up.</>} />
        ),
        changes: [
          ...(exists ? [] : [
            <>ModelRegistry <code>{MODEL_REGISTRY_NAME}</code> is created in {where(mr)} with your OpenShift account, with the default database (<code>spec.postgres.generateDeployment: true</code>); its operator creates the server, a PostgreSQL database with a 5 GiB volume, and a Service.</>,
            <>This tool records the registry&apos;s UID in its own namespace, and labels it.</>,
          ]),
          ...(samples || exists ? [<>The registry&apos;s REST API on the cluster&apos;s gateway, with your token: <SampleName />, and its model artifact pointing at the Models tab&apos;s weights.</>] : []),
          <>No access is granted to anyone else.</>,
        ],
      };
    }
    case "teardown-model-registry":
      return {
        title: "Delete the model registry?", confirm: "Delete", danger: true, runTitle: "Delete the model registry",
        summary: <Content component="p">Deletes model registry {MODEL_REGISTRY_NAME}, with every model registered in it.</Content>,
        changes: [
          <>ModelRegistry <code>{MODEL_REGISTRY_NAME}</code> is deleted, only if it is the one this tool created (its UID).</>,
          <>Its operator and the garbage collector delete its server and its default database: Deployment, Service, Secret and volume.</>,
          <>The DataScienceCluster component stays on.</>,
        ],
        dataLoss: <>Every registered model, version and artifact record in the registry, and its database. Model files stored elsewhere are not touched.</>,
      };
    default:
      return null;
  }
}
