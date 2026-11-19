import React from "react";
import {
  ActionListItem,
  Alert,
  AlertActionLink,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { InferenceGatewayState } from "../../types";
import { ImageRef } from "../ImageRef";
import { type Pending, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { PartItem, type PartMenuItem } from "./FeatureLayout";
import { notManaged } from "./resourceStatus";
import { READ_ONLY_SETUP } from "./PipelinesTab";
import { gatewayStatus, foreignGatewayStep } from "./featureState";
import type { ModelsTabProps } from "./ModelsTab";

/** The inference gateway's objects (pkg/cluster inference_gateway.go). */
export const GATEWAY = "openshift-ai-inference";
export const GATEWAY_NS = "openshift-ingress";

export const gatewayManaged = (gw: InferenceGatewayState | undefined) => gw?.managedByTool === true;

export const AUTH_WORDS: Record<string, string> = {
  enforced: "On: every call needs an OpenShift token whose user can see the model's project",
  "not-enforced": "Not enforced yet",
  missing: "Added by RHOAI when the first model uses the gateway",
};

/**
 * Models (anyone's, such as one deployed from the dashboard) that route
 * through the gateway from a project it does not list: their routes are
 * rejected, so they never become Ready. For this tool's gateway, Allow
 * adds the project to its list; for someone else's, RHOAI's documented
 * step for a cluster admin.
 */
const NotAllowedAlert: React.FC<{ gw: InferenceGatewayState; project: string; models: string[]; baseReason: string | null; readOnly: boolean; setPending: (p: Pending) => void }> = ({ gw, project, models, baseReason, readOnly, setPending }) => {
  const one = models.length === 1;
  const title = `${models.join(", ")} can't be reached: ${one ? "its" : "their"} project isn't allowed on the inference gateway`;
  const why = `${one ? "Its route is" : "Their routes are"} rejected because ${project} is not on the gateway's list of projects, so the dashboard shows ${one ? "it" : "them"} as not ready.`;
  if (!gatewayManaged(gw)) {
    return (
      <Alert variant="warning" isInline component="div" title={title}>
        <Content component="p">{why} {foreignGatewayStep(project)}</Content>
      </Alert>
    );
  }
  const reason = readOnly ? "A cluster-admin makes this change." : baseReason;
  return (
    <Alert variant="warning" isInline component="div" title={title}
      actionLinks={!reason ? <AlertActionLink onClick={() => setPending({ kind: "allow-gateway-project", project })}>Allow {project}</AlertActionLink> : undefined}>
      <Content component="p">{why} Allow adds {project} to the list. Allow only a project you trust: anyone who can edit {project} can then attach routes to this shared gateway, which carries every model's calls and their tokens.</Content>
      {reason && <Content component="p">{readOnly ? reason : `Not available now: ${reason}`}</Content>}
    </Alert>
  );
};

/** The models the gateway does not allow, by project, in the projects' order. */
function notAllowedByProject(gw: InferenceGatewayState): [string, string[]][] {
  const out = new Map<string, string[]>();
  for (const ref of gw.notAllowedModels ?? []) {
    const ns = ref.split("/")[0];
    out.set(ns, [...(out.get(ns) ?? []), ref]);
  }
  return [...out.entries()];
}

/** The projects the gateway allows, in words. */
function allowedWords(gw: InferenceGatewayState): React.ReactNode {
  if (gw.allProjectsAllowed) return "Every project";
  if (!gw.allowedProjects) return "Chosen by its namespace selector";
  if (gw.allowedProjects.length === 0) return "None";
  return gw.allowedProjects.map((p, i) => <React.Fragment key={p}>{i > 0 ? ", " : ""}<code>{p}</code></React.Fragment>);
}

/**
 * Projects on this tool's gateway's list that no model uses (their editors
 * can still attach routes to the shared gateway), first those that no
 * longer exist (a new project of that name would be admitted at once), each
 * with Remove from the list.
 */
const UnusedProjectsAlert: React.FC<{ gw: InferenceGatewayState; reason: string | null; setPending: (p: Pending) => void }> = ({ gw, reason, setPending }) => {
  const gone = gw.goneProjects ?? [];
  const unused = (gw.unusedProjects ?? []).filter((p) => !gone.includes(p));
  if (gone.length === 0 && unused.length === 0) return null;
  const row = (p: string, isGone: boolean) => (
    <StackItem key={p}>
      <code>{p}</code> — {isGone ? "deleted" : "no model uses the gateway"}{" "}
      <Button variant="link" isInline isDanger isAriaDisabled={!!reason} aria-label={`Remove ${p} from the inference gateway`} onClick={() => setPending({ kind: "disallow-gateway-project", project: p, gone: isGone })}>Remove</Button>
    </StackItem>
  );
  return (
    <Alert variant={gone.length > 0 ? "warning" : "info"} isInline component="div" title="The gateway accepts projects that don't use it">
      <Stack hasGutter>
        <StackItem>
          <Content component="p">
            Anyone who can edit a project on the list can attach routes to this shared gateway, and a route there can take another model&apos;s calls and tokens.
            {gone.length > 0 ? " A project created later with a deleted project's name would be accepted at once." : ""}
          </Content>
        </StackItem>
        {gone.map((p) => row(p, true))}
        {unused.map((p) => row(p, false))}
        {reason && <StackItem><Content component="small">Not available now: {reason}</Content></StackItem>}
      </Stack>
    </Alert>
  );
};

/** The inference gateway (cluster). */
export const GatewayPart: React.FC<ModelsTabProps> = ({ status, busy, baseReason, readOnly, setPending }) => {
  const gw = status.inferenceGateway;
  const unmanaged = notManaged(gw);
  const deployed = !!gw?.deployed;
  const menu: PartMenuItem[] = !readOnly && gatewayManaged(gw) && !isTerminating(gw)
    ? [{ key: "delete", label: "Delete inference gateway...", onClick: () => setPending({ kind: "teardown-gateway" }), disabledReason: baseReason ?? gw?.teardownBlockedReason ?? null, isDanger: true }]
    : [];
  const s = gatewayStatus(status, busy);
  const setUpReason = readOnly ? READ_ONLY_SETUP : baseReason ?? gw?.setupBlockedReason ?? null;
  return (
    <PartItem id="gateway" title="Inference gateway" scope="Shared by the cluster" status={s} unmanaged={unmanaged} menu={menu}
      actions={!deployed && s === "not-set-up" && !readOnly ? (
        <ActionListItem>
          <Button variant="secondary" size="sm" isAriaDisabled={!!setUpReason} onClick={() => setPending({ kind: "setup-models", gateway: true, project: null, model: "" })}>Set up the inference gateway</Button>
        </ActionListItem>
      ) : undefined}>
      <Content component="p">
        A front door for models from outside the cluster: it answers at <code>{gw?.externalUrl ?? gw?.plannedUrl ?? "https://inference.<apps domain>"}</code>, sends each call to its model, and checks the caller&apos;s OpenShift token.
        {unmanaged
          ? " It was created outside this tool (for example by RHOAI's installation): models use it as it is, and this tool does not change or delete it."
          : <>
            {gw?.allProjectsAllowed ? null : " It accepts models only from the projects on its list, as RHOAI documents it; a model set up through it adds its project, and deleting a project's last model takes the project off again."}
            {deployed ? " It runs on a cloud load balancer, which costs money while it exists." : " A model does not need it: MaaS, another gateway or the cluster's inside are other ways. Setting it up creates a cloud load balancer, which costs money while it exists."}
          </>}
      </Content>
      {gw && <ProblemAlerts kind="gateway" state={gw} />}
      {gw?.deployed && gatewayManaged(gw) && !isTerminating(gw) && <UnusedProjectsAlert gw={gw} reason={readOnly ? READ_ONLY_SETUP : baseReason} setPending={setPending} />}
      {gw?.deployed && !isTerminating(gw) && s !== "setting-up" && notAllowedByProject(gw).map(([ns, models]) => (
        <NotAllowedAlert key={ns} gw={gw} project={ns} models={models} baseReason={baseReason} readOnly={readOnly} setPending={setPending} />
      ))}
      {deployed && s === "setting-up" && !terminalReason(gw) && !isTerminating(gw) && <StartingNote state={gw!} typical="under a minute" />}
      {deployed && (
        <DescriptionList isCompact isHorizontal aria-label="Inference gateway details">
          <DescriptionListGroup>
            <DescriptionListTerm>Gateway</DescriptionListTerm>
            <DescriptionListDescription><code>{gw?.namespace ?? GATEWAY_NS}/{gw?.name ?? GATEWAY}</code></DescriptionListDescription>
          </DescriptionListGroup>
          {gw?.externalUrl && (
            <DescriptionListGroup>
              <DescriptionListTerm>External address</DescriptionListTerm>
              <DescriptionListDescription>
                <ImageRef image={gw.externalUrl} what="external address" truncate={false} isCode={false} />
              </DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {gw?.dns && (
            <DescriptionListGroup>
              <DescriptionListTerm>DNS</DescriptionListTerm>
              <DescriptionListDescription>{gw.dns === "published" ? "Published" : "Not published yet; a new load balancer's name can take a few minutes"}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {gw?.ready && (
            <DescriptionListGroup>
              <DescriptionListTerm>Authentication</DescriptionListTerm>
              <DescriptionListDescription>{gw.authPolicy ? AUTH_WORDS[gw.authPolicy] : "Unknown"}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {gw && (
            <DescriptionListGroup>
              <DescriptionListTerm>Projects allowed on the gateway</DescriptionListTerm>
              <DescriptionListDescription>{allowedWords(gw)}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
          {(gw?.usedBy ?? []).length > 0 && (
            <DescriptionListGroup>
              <DescriptionListTerm>Used by</DescriptionListTerm>
              <DescriptionListDescription>{(gw?.usedBy ?? []).map((u, i) => <React.Fragment key={u}>{i > 0 ? ", " : ""}<code>{u}</code></React.Fragment>)}</DescriptionListDescription>
            </DescriptionListGroup>
          )}
        </DescriptionList>
      )}
    </PartItem>
  );
};
