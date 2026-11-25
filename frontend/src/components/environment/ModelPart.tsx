import React from "react";
import {
  Alert,
  AlertActionLink,
  Button,
  Content,
  Label,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
  Tooltip,
} from "@patternfly/react-core";
import type { CatalogModel, ModelCallCheck, ModelState } from "../../types";
import { ImageRef } from "../ImageRef";
import { ExternalLink, ProblemAlerts, StartingNote, isTerminating, terminalReason } from "./resourceRow";
import { PartItem, type PartMenuItem } from "./FeatureLayout";
import { notManaged } from "./resourceStatus";
import { callCheckFailed, catalogModel, dashboardProjectHref, modelIn, modelStatus, shortModelName } from "./featureState";
import type { ModelsTabProps } from "./ModelsTab";
import { AUTH_WORDS } from "./GatewayPart";
import { modelNeeds, modelUseWords } from "./ModelPicker";
import { exposureWords } from "./ExposureChooser";

/** Which image a model runs, in words. */
function imageWords(m: ModelState): string {
  switch (m.imageKind) {
    case "released":
      return `Red Hat vLLM CPU${m.imageVersion ? ` ${m.imageVersion}` : ""}, the newest release (this nightly build's preset image is not published yet)`;
    case "override":
      return "The image VLLM_CPU_IMAGE names";
    default:
      return "Red Hat vLLM CPU, the cluster preset's image";
  }
}

/** How long ago an RFC 3339 time was, in words ("2 minutes ago"). */
export function agoWords(at: string, now: number = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(at).getTime()) / 1000));
  if (Number.isNaN(s)) return "at an unknown time";
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} ${m === 1 ? "minute" : "minutes"} ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} ${h === 1 ? "hour" : "hours"} ago`;
  const d = Math.round(h / 24);
  return `${d} ${d === 1 ? "day" : "days"} ago`;
}

/** The last check: its label and when; brief leaves out the message (a failed check's alert above the details says it). */
const CallCheckWords: React.FC<{ check: ModelCallCheck | undefined; brief?: boolean }> = ({ check, brief }) => {
  if (!check) return <>Not checked since this app started. Check again calls the model once.</>;
  // A failed check needs attention (orange, as the part's status), never an error label.
  const color = check.ok ? "green" : check.result === "not-checked" ? "grey" : "orange";
  const word = check.ok ? (check.result === "maas" ? "Listed by MaaS" : "Answers requests")
    : check.result === "not-checked" ? "Not checked" : check.result === "unauthenticated" ? "Answers without a token" : "Cannot be called";
  return (
    <Stack>
      <StackItem>
        <Label isCompact color={color}>{word}</Label>{" "}
        <Tooltip content={new Date(check.at).toLocaleString()}><Content component="small" tabIndex={0}>Checked {agoWords(check.at)}</Content></Tooltip>
      </StackItem>
      {!brief && <StackItem>{check.message}</StackItem>}
    </Stack>
  );
};

/** What the model is, in a sentence for the part. */
function modelPurpose(c: CatalogModel | undefined): string {
  if (!c) return "A model served by vLLM on CPU.";
  if (c.type === "embedding") return `An embedding model: it turns text into vectors of ${c.embeddingDimension ?? "fixed"} numbers, for semantic search and retrieval. It cannot chat.`;
  if (c.toolCalling) return "A chat model that can call tools you describe in a request. Its reasoning (thinking) is off unless a request turns it on.";
  return c.useCase.includes("smoke") ? "A very small chat model, to check quickly that model serving works. Its answers are simple." : "A small chat model.";
}

/** The chosen model in the selected project, or a placeholder until a project is chosen. */
export const ModelPart: React.FC<ModelsTabProps> = ({ status, project, model: name, busy, baseReason, readOnly, setPending, onCheckModel, checking, checkError }) => {
  const c = catalogModel(status, name);
  const title = c?.displayName ?? name;
  if (!project) {
    return (
      <PartItem id="model" title={title} scope="In the project you choose" status="not-set-up" unmanaged={false}>
        <Content component="p">Choose a project above to see this model there, or to set it up.</Content>
      </PartItem>
    );
  }
  const model = modelIn(status, project, name);
  const unmanaged = notManaged(model);
  const managed = !!model && model.managedByTool !== false;
  const href = dashboardProjectHref(status.dashboardUrl, project);
  const menu: PartMenuItem[] = managed && !readOnly && !isTerminating(model)
    ? [{ key: "delete", label: "Delete model", onClick: () => setPending({ kind: "teardown-model", project, model: name, ...(model?.maasOnlySetup ? { unpublish: true } : {}) }), disabledReason: baseReason ?? null, isDanger: true }]
    : [];
  const useReleased = () => setPending({ kind: "setup-models", gateway: false, project, model: name, releasedImage: true });
  const s = modelStatus(status, project, busy, name);
  const isChecking = !!checking && checking.project === project && checking.model === name;
  const checkAgain = () => (onCheckModel ? onCheckModel(project, name) : setPending({ kind: "check-model", project, model: name }));
  const checkButton = (
    <Button variant="secondary" size="sm" isLoading={isChecking} isAriaDisabled={isChecking || !!baseReason} onClick={checkAgain}>{isChecking ? "Checking" : "Check again"}</Button>
  );
  const shortName = shortModelName(status, name);
  return (
    <PartItem id="model" title={model?.displayName || title} scope={`In project ${project}`} status={s} unmanaged={unmanaged} menu={menu}>
      {managed && model?.ready && !isTerminating(model) && callCheckFailed(model) && (
        <Alert variant="warning" isInline component="div" title={model.callCheck?.result === "unauthenticated" ? `${shortName} answers without a token` : `${shortName} is running but can't be called`}
          actionLinks={!readOnly && !baseReason && !isChecking ? <AlertActionLink onClick={checkAgain}>Check again</AlertActionLink> : undefined}>
          <Content component="p">{model.callCheck?.message}</Content>
        </Alert>
      )}
      {unmanaged
        ? <Content component="p">A model named <code>{model?.name}</code> was created outside this tool (for example in the dashboard); this tool leaves it alone.</Content>
        : model
          ? <Content component="p">{modelPurpose(c)} It runs on vLLM on CPU in {project} and reserves {modelNeeds(c)}. The dashboard lists it with the project&apos;s deployments{c?.type === "embedding" ? "." : ", and its Gen AI playground can chat with it."}{model.ready && href && !isTerminating(model) ? <> <ExternalLink href={href} isInline>Open {project} in the dashboard</ExternalLink></> : null}</Content>
          : <Content component="p">{modelPurpose(c)} Not set up in {project} yet: it needs {modelNeeds(c)} on one worker, downloads about {c?.downloadSize ?? "1 GB"}, and takes 3 to 5 minutes the first time.</Content>}
      {model?.presetImageMissing && managed && (
        <Alert variant="danger" isInline component="div" title={model.presetImageUncertain ? "The cluster preset's image may not be published" : "The cluster preset's image is not published yet"}
          actionLinks={!readOnly && !baseReason ? <AlertActionLink onClick={useReleased}>Use the newest released Red Hat image</AlertActionLink> : undefined}>
          {model.presetImageUncertain && (
            <Content component="p">
              The image pull has failed for several minutes. One registry answered that the image does not exist, and another refused the node&apos;s credentials, so either the image is not published or the node lacks the pull secret. Details: {model.message}
            </Content>
          )}
          <Content component="p">
            {model.presetImageUncertain ? "If it is not published: this" : "This"} cluster runs a nightly RHOAI build, whose CPU preset names a runtime image Red Hat has not published, so the model cannot start.
            Use the newest released Red Hat vLLM CPU image instead: only the model&apos;s image changes. To use the preset&apos;s own image, the cluster needs a mirror of <code>registry.redhat.io/rhaii</code> to <code>registry.stage.redhat.io/rhaii</code> and a credential for that registry.
          </Content>
          {readOnly ? <Content component="p">A cluster-admin makes this change.</Content> : baseReason && <Content component="p">Not available now: {baseReason}</Content>}
        </Alert>
      )}
      {model && !model.presetImageMissing && <ProblemAlerts kind="model" state={model} />}
      {model && s === "setting-up" && !terminalReason(model) && !isTerminating(model) && <StartingNote state={model} typical="3 to 5 minutes the first time" />}
      <DescriptionList isCompact isHorizontal aria-label="Model details">
        <DescriptionListGroup>
          <DescriptionListTerm>Name</DescriptionListTerm>
          <DescriptionListDescription><code>{model?.name ?? name}</code></DescriptionListDescription>
        </DescriptionListGroup>
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>Use case</DescriptionListTerm>
            <DescriptionListDescription>{modelUseWords(c)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reserves</DescriptionListTerm>
            <DescriptionListDescription>{modelNeeds(c)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>From</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack>
                <StackItem><ImageRef image={c.uri} what="model source" isCode /></StackItem>
                <StackItem><Content component="small">Hugging Face, pinned to one commit; about {c.downloadSize} downloaded</Content></StackItem>
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {c && (
          <DescriptionListGroup>
            <DescriptionListTerm>vLLM options</DescriptionListTerm>
            <DescriptionListDescription><code>{c.args}</code></DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.exposure && (
          <DescriptionListGroup>
            <DescriptionListTerm>Reached</DescriptionListTerm>
            <DescriptionListDescription>{exposureWords(model)}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.url && model.exposure !== "internal" && (
          <DescriptionListGroup>
            <DescriptionListTerm>{model.exposure === "maas" ? "MaaS address" : "Address"}</DescriptionListTerm>
            <DescriptionListDescription>
              <ImageRef image={model.url} what={model.exposure === "maas" ? "MaaS address" : "model address"} truncate={false} isCode={false} />
              {model.exposure === "maas" && <HelperText><HelperTextItem>Call it with a MaaS API key: see the MaaS tab&apos;s commands.</HelperTextItem></HelperText>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.internalUrl && (
          <DescriptionListGroup>
            <DescriptionListTerm>In-cluster address</DescriptionListTerm>
            <DescriptionListDescription>
              <ImageRef image={model.internalUrl} what="in-cluster address" truncate={false} isCode={false} />
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.servedName && (
          <DescriptionListGroup>
            <DescriptionListTerm>Model name in requests</DescriptionListTerm>
            <DescriptionListDescription><code>{model.servedName}</code> (send exactly this as <code>&quot;model&quot;</code>; the display name or the Hugging Face id gets 404)</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {model?.gatewayAuth && (
          <DescriptionListGroup>
            <DescriptionListTerm>Gateway authentication</DescriptionListTerm>
            <DescriptionListDescription>{model.gatewayAuth === "maas" ? "MaaS checks each call's API key and its subscription" : AUTH_WORDS[model.gatewayAuth]}</DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && model?.ready && !isTerminating(model) && (
          <DescriptionListGroup>
            <DescriptionListTerm>Can it be called?</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack hasGutter>
                <StackItem><CallCheckWords check={model.callCheck} brief={callCheckFailed(model)} /></StackItem>
                {!readOnly && !callCheckFailed(model) && <StackItem>{checkButton}</StackItem>}
                {!readOnly && checkError && !isChecking && <StackItem><HelperText><HelperTextItem variant="warning">{checkError}</HelperTextItem></HelperText></StackItem>}
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Runtime image</DescriptionListTerm>
            <DescriptionListDescription>
              <Stack>
                <StackItem>{imageWords(model)}</StackItem>
                {model.currentImage && <StackItem><ImageRef image={model.currentImage} what="runtime image" /></StackItem>}
              </Stack>
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Hardware profile</DescriptionListTerm>
            <DescriptionListDescription>
              {model.hardwareProfile
                ? <><code>{model.hardwareProfile}</code>, as the dashboard shows it</>
                : <>None fits: no hardware profile on this cluster allows {modelNeeds(c)}, so the dashboard's Deployments table shows &quot;No hardware profile&quot; and the model&apos;s details &quot;Hardware profile: Unknown&quot;. The model runs as set up.</>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
        {managed && (
          <DescriptionListGroup>
            <DescriptionListTerm>Token authentication</DescriptionListTerm>
            <DescriptionListDescription>
              {model.tokenAuthOff
                ? <>Off: security.opendatahub.io/enable-auth is &quot;false&quot; on the model (set in the dashboard), so its address answers without a token. Turn &quot;Require token authentication&quot; back on in the dashboard&apos;s Edit.</>
                : model.tokenSecret
                  ? <>On, as the dashboard sets it up: ServiceAccount {model.tokenServiceAccount} may only get this model, and its token is in Secret <code>{model.tokenSecret}</code>, which the dashboard lists as the model&apos;s token &quot;default-name&quot;.</>
                  : <>On, but the model has no token of its own yet, so the dashboard lists none and its Edit would turn authentication off when saved. Set it up again to add the model&apos;s token.</>}
            </DescriptionListDescription>
          </DescriptionListGroup>
        )}
      </DescriptionList>
    </PartItem>
  );
};
