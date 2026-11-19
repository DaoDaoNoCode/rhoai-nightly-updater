import React, { useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Checkbox,
  CodeBlock,
  CodeBlockCode,
  Content,
  ExpandableSection,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Radio,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";
import type { DSCPendingHardReset, DSCPlan, OperationResponse, UpdateStep } from "../../types";
import { finishDSCHardReset, getDSCPlan, runDSCPlan, streamDSCHardReset, toApiError } from "../../services/api";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { TooltipButton } from "../TooltipButton";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { errorResult, outcomeTitle, outcomeVariant } from "../../outcomes";
import { BlockedAlert, PathList, WarningsAlert, changeText, durationText } from "./planText";

type HardStep = { id: string; title: string };
const HARD_RESET_STEPS: HardStep[] = [
  { id: "check", title: "Check the DataScienceCluster, the operator and every component" },
  { id: "delete", title: "Delete the DataScienceCluster" },
  { id: "wait_deleted", title: "Wait for the operator to remove every component" },
  { id: "create", title: "Create the release default" },
  { id: "wait_ready", title: "Wait for the components" },
];

/** Reset to the release default: soft (in place) or hard (delete and recreate). */
export const ResetModal: React.FC<{
  name: string;
  disabledReason: string | null;
  onClose: () => void;
  onDone: (res: OperationResponse) => void;
}> = ({ name, disabledReason, onClose, onDone }) => {
  const titleId = React.useId();
  const [kind, setKind] = useState<"soft-reset" | "hard-reset">("soft-reset");
  // Keep the components the user turned on: checked by default; the plan is
  // made again when it changes.
  const [keep, setKeep] = useState(true);
  // The toggles the user turned on and the fields a reset keeps, from the
  // last plan (kept while a new plan loads, so the options do not flicker).
  const [turnedOn, setTurnedOn] = useState<string[]>([]);
  const [protectedFields, setProtectedFields] = useState<string[]>([]);
  const [plan, setPlan] = useState<DSCPlan | null>(null);
  const [planError, setPlanError] = useState("");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [steps, setSteps] = useState<Record<string, StepState>>({});
  const [result, setResult] = useState<OperationResponse | null>(null);
  // What the reset would do: open unless it is blocked, until the user
  // toggles it (null: the default for the plan shown).
  const [detailsOpen, setDetailsOpen] = useState<boolean | null>(null);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const started = Object.keys(steps).length > 0;

  useEffect(() => {
    let active = true;
    setPlan(null);
    setPlanError("");
    setDetailsOpen(null);
    getDSCPlan(kind, undefined, { keepEnabled: keep })
      .then((p) => { if (active) { setPlan(p); setTurnedOn(p.turnedOn ?? []); setProtectedFields(p.protectedFields ?? []); } })
      .catch((e) => { if (active) setPlanError(toApiError(e).message); });
    return () => { active = false; };
  }, [kind, keep]);

  const hasBlockers = (plan?.blocked?.length ?? 0) > 0;
  const blocked = !plan || !!plan.refusal || hasBlockers;
  const confirm = async () => {
    if (!plan || blocked) return;
    setBusy(true);
    if (kind === "soft-reset") {
      let res: OperationResponse;
      try {
        res = await runDSCPlan("soft-reset", plan);
      } catch (e) {
        res = errorResult(e, "Could not reset the DataScienceCluster");
      }
      setBusy(false);
      onDone(res);
      onClose();
      return;
    }
    let final = "";
    controller.current = streamDSCHardReset(plan, typed,
      (step: UpdateStep) => {
        if (step.step === "operation_complete") { final = step.message; return; }
        setSteps((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
      },
      (success, error, apiError) => {
        const res: OperationResponse = { success, message: success ? final : error || final || "The hard reset failed.", logs: [], errorCode: apiError?.errorCode };
        setBusy(false);
        setResult(res);
        onDone(res);
      },
      () => {
        const res: OperationResponse = { success: false, message: "The connection to the server was lost. The hard reset continues on the server; this page shows the DataScienceCluster when it ends.", logs: [], errorCode: "detached" };
        setBusy(false);
        setResult(res);
        onDone(res);
      });
  };

  const kept = plan?.kept ?? [];
  // A blocked removal of a component the user turned on: keeping them avoids it.
  const keepWouldHelp = !keep && (plan?.blocked ?? []).some((b) => turnedOn.includes(b.component));
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={() => !busy && onClose()}>
      <ModalHeader title="Reset to the release default" labelId={titleId} titleIconVariant={kind === "hard-reset" ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {!started && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup role="radiogroup" isStack fieldId="dsc-reset-kind" label="How">
                  <Radio id="dsc-reset-soft" name="dsc-reset-kind" isChecked={kind === "soft-reset"} onChange={() => setKind("soft-reset")}
                    label="Soft reset (recommended)"
                    description={<>Changes <code>{name}</code> in place: only the components and settings that differ from the release default.</>} />
                  <Radio id="dsc-reset-hard" name="dsc-reset-kind" isChecked={kind === "hard-reset"} onChange={() => setKind("hard-reset")}
                    label="Hard reset"
                    description={<>Deletes <code>{name}</code> and creates the release default in its place. Use it when components are stuck and a soft reset does not help.</>} />
                  {protectedFields.length > 0 && (
                    <FormHelperText>
                      <HelperText><HelperTextItem>Both keep the value of <PathList paths={protectedFields} />.</HelperTextItem></HelperText>
                    </FormHelperText>
                  )}
                </FormGroup>
                {turnedOn.length > 0 && (
                  <Checkbox id="dsc-reset-keep" isChecked={keep} onChange={(_e, v) => setKeep(v)}
                    label={<>Keep the components you turned on ({turnedOn.length}): <PathList paths={turnedOn} /></>}
                    description="Enabled now, but off in the release default. Unchecked, the reset turns them off." />
                )}
              </Form>
            </StackItem>
          )}
          {planError && <StackItem><Alert component="p" variant="danger" isInline title="Could not work out the reset">{planError}</Alert></StackItem>}
          {!plan && !planError && <StackItem><Spinner size="md" aria-label="Working out the reset" /></StackItem>}
          {plan?.refusal && <StackItem><Alert component="p" variant={plan.code === "nothing_to_do" ? "info" : "warning"} isInline title={plan.code === "nothing_to_do" ? "Nothing to change" : "Not possible now"}>{plan.refusal}</Alert></StackItem>}
          {plan && !plan.refusal && !started && (
            <>
              {hasBlockers && <StackItem><BlockedAlert plan={plan} /></StackItem>}
              {keepWouldHelp && (
                <StackItem>
                  <Content component="p">Check <strong>Keep the components you turned on</strong> to reset everything else and keep these as they are.</Content>
                </StackItem>
              )}
              <StackItem>
                <ExpandableSection toggleText={kind === "hard-reset" ? "What a hard reset would do" : "What a soft reset would do"}
                  isExpanded={detailsOpen ?? !hasBlockers} onToggle={(_e, v) => setDetailsOpen(v)}>
                  {kind === "hard-reset" ? <HardResetDetails plan={plan} name={name} kept={kept} /> : <SoftResetDetails plan={plan} kept={kept} />}
                </ExpandableSection>
              </StackItem>
            </>
          )}
          {plan && !plan.refusal && !started && kind === "hard-reset" && !blocked && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label={<>Type <code>{name}</code> to confirm</>} fieldId="dsc-hard-reset-confirm" isRequired>
                  <TextInput id="dsc-hard-reset-confirm" value={typed} onChange={(_e, v) => setTyped(v)} autoComplete="off" />
                </FormGroup>
              </Form>
            </StackItem>
          )}
          {started && (
            <StackItem>
              <StepProgress idPrefix="dsc-hard-reset" aria-label="Hard reset progress" steps={HARD_RESET_STEPS} states={steps} running={busy}
                shownElsewhere={result && !result.success ? result.message : undefined} />
            </StackItem>
          )}
          {result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "The hard reset stopped")}>{result.success ? undefined : result.message}</Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {result || started ? (
          <Button variant="primary" onClick={onClose} isDisabled={busy}>Close</Button>
        ) : (
          <>
            <TooltipButton variant={kind === "hard-reset" ? "danger" : "primary"} onClick={confirm} isLoading={busy}
              isDisabled={busy || blocked || !!disabledReason || (kind === "hard-reset" && typed !== name)} disabledReason={disabledReason}>
              {kind === "hard-reset" ? "Hard reset" : "Reset"}
            </TooltipButton>
            <Button variant="link" onClick={onClose} isDisabled={busy}>Cancel</Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};

/** A hard reset that deleted the DataScienceCluster and did not create its replacement, while none exists. */
export const FinishHardReset: React.FC<{
  pending?: DSCPendingHardReset;
  error?: string;
  disabledReason: string | null;
  onDone: (res: OperationResponse) => void;
}> = ({ pending, error, disabledReason, onDone }) => {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [yamlOpen, setYamlOpen] = useState(false);
  const finish = async () => {
    if (!pending) return;
    setBusy(true);
    let res: OperationResponse;
    try {
      res = await finishDSCHardReset(pending.name, pending.token);
    } catch (e) {
      res = errorResult(e, "Could not finish the hard reset");
    }
    setBusy(false);
    setConfirming(false);
    onDone(res);
  };
  return (
    <PageSection>
      <Card>
        <CardHeader actions={pending ? {
          actions: (
            <TooltipButton variant="primary" onClick={() => setConfirming(true)} isDisabled={!!disabledReason || busy} disabledReason={disabledReason}>
              Finish the hard reset…
            </TooltipButton>
          ),
          hasNoOffset: false,
        } : undefined}>
          <CardTitle><Title headingLevel="h2" size="lg">A hard reset did not finish</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            {error && <StackItem><Alert component="p" variant="danger" isInline title="Could not read the recorded hard reset">{error}</Alert></StackItem>}
            {pending && (
              <>
                <StackItem>
                  <Content component="p">
                    A hard reset{pending.user ? <> by {pending.user}</> : null} at {pending.startedAt} deleted DataScienceCluster <code>{pending.name}</code> and did not create its replacement.
                    Finish it to create exactly the object it planned: the release default of operator {pending.operatorVersion || "(unknown)"}, with the old name and namespaces.
                    Create DataScienceCluster would create the release default without the old name and namespaces instead.
                  </Content>
                </StackItem>
                <StackItem>
                  <ExpandableSection toggleText="The DataScienceCluster it creates" isExpanded={yamlOpen} onToggle={(_e, v) => setYamlOpen(v)}>
                    <CodeBlock><CodeBlockCode>{pending.newObject}</CodeBlockCode></CodeBlock>
                  </ExpandableSection>
                </StackItem>
              </>
            )}
          </Stack>
        </CardBody>
      </Card>
      {pending && confirming && (
        <ConfirmActionModal
          isOpen
          title="Finish the hard reset?"
          changes={[<>Creates DataScienceCluster <code>{pending.name}</code> ({pending.apiVersion}) exactly as the hard reset planned it.</>]}
          changesOpen
          confirmLabel={`Create ${pending.name}`}
          isLoading={busy}
          confirmDisabled={busy || !!disabledReason}
          onConfirm={finish}
          onCancel={() => !busy && setConfirming(false)}
        >
          <Content component="p" className="pf-v6-u-text-color-subtle">
            The server checks again that no DataScienceCluster exists and that the operator is the one the reset planned for, then runs a dry run first.
          </Content>
        </ConfirmActionModal>
      )}
    </PageSection>
  );
};

/** A collapsed section of details (controlled, so its toggle always says whether it is expanded). */
const Details: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => {
  const [open, setOpen] = useState(false);
  return <ExpandableSection toggleText={title} isExpanded={open} onToggle={(_e, v) => setOpen(v)}>{children}</ExpandableSection>;
};

/** What a soft reset changes: the list of changes, what it keeps, and its warnings. */
const SoftResetDetails: React.FC<{ plan: DSCPlan; kept: string[] }> = ({ plan, kept }) => {
  const removals = plan.changes.filter((c) => c.effect === "removes").length;
  const deploys = plan.changes.filter((c) => c.effect === "deploys").length;
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">This changes the shared cluster:</Content>
        <List aria-label="Changes of the reset">{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
        {kept.length > 0 && <Content component="p" className="pf-v6-u-mt-sm">Kept: <PathList paths={kept} /> (you turned these on).</Content>}
      </StackItem>
      {(plan.warnings?.length ?? 0) > 0 && <StackItem><WarningsAlert plan={plan} /></StackItem>}
      {(removals > 0 || deploys > 0) && (
        <StackItem>
          <Content component="p" className="pf-v6-u-text-color-subtle">
            {removals > 0 && <>{removals} {removals === 1 ? "component is" : "components are"} removed. </>}
            {deploys > 0 && <>{deploys} {deploys === 1 ? "component is" : "components are"} deployed.</>}
          </Content>
        </StackItem>
      )}
    </Stack>
  );
};

/**
 * What a hard reset does, the decision first: a short summary, what is kept
 * or stays off, then each group of details (from the plan) on demand.
 */
const HardResetDetails: React.FC<{ plan: DSCPlan; name: string; kept: string[] }> = ({ plan, name, kept }) => {
  const tornDown = plan.tornDown ?? [];
  const stayRemoved = plan.stayRemoved ?? [];
  const stops = plan.stopsWorking ?? [];
  const crds = plan.conversionCRDs ?? [];
  const within = plan.deleteTimeoutSeconds ? `within ${durationText(plan.deleteTimeoutSeconds)}` : "in time";
  return (
    <Stack hasGutter>
      <StackItem>
        <Alert component="div" variant="danger" isInline
          title={<>Deletes <code>{name}</code> and tears down every enabled component ({tornDown.length})</>}>
          <Content component="p">
            They come back as the release default; RHOAI is unavailable for minutes. If the old DataScienceCluster is not gone {within}, nothing is created and this page offers to finish later.
          </Content>
        </Alert>
      </StackItem>
      {(stayRemoved.length > 0 || kept.length > 0) && (
        <StackItem>
          {stayRemoved.length > 0 && <Content component="p">Stay Removed (on now, off in the release default): <PathList paths={stayRemoved} />. Turn them on again afterwards if you need them.</Content>}
          {kept.length > 0 && <Content component="p">Kept: <PathList paths={kept} /> (you turned these on). They come back as they are now.</Content>}
        </StackItem>
      )}
      {(plan.warnings?.length ?? 0) > 0 && <StackItem><WarningsAlert plan={plan} /></StackItem>}
      <StackItem>
        {/* The details, collapsed: one toggle under the other. */}
        <Stack>
          {stops.length > 0 && (
            <StackItem>
              <Details title={`Stops working until its component is back (${stops.length})`}>
                <List aria-label="What stops working until its component is back">
                  {stops.map((s) => <ListItem key={s.resource}>{s.resource}: needs <PathList paths={s.components} /></ListItem>)}
                </List>
              </Details>
            </StackItem>
          )}
          {crds.length > 0 && (
            <StackItem>
              <Details title={`CRDs that cannot be read at another version meanwhile (${crds.length})`}>
                <List aria-label="CRDs that cannot be read at another version meanwhile">
                  {crds.map((c) => <ListItem key={c.crd}><code>{c.crd}</code>: served by <code>{c.component}</code></ListItem>)}
                </List>
              </Details>
            </StackItem>
          )}
          <StackItem>
            {plan.changes.length > 0 ? (
              <Details title={`Changes in the new DataScienceCluster (${plan.changes.length})`}>
                <List aria-label="Differences of the new DataScienceCluster">{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
              </Details>
            ) : (
              <Content component="p">Changes in the new DataScienceCluster: none; it has the same settings as now.</Content>
            )}
          </StackItem>
          <StackItem>
            <Details title="The DataScienceCluster it creates">
              <CodeBlock><CodeBlockCode>{plan.newObject}</CodeBlockCode></CodeBlock>
            </Details>
          </StackItem>
        </Stack>
      </StackItem>
    </Stack>
  );
};
