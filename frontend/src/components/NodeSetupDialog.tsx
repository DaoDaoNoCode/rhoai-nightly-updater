import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import type { NodeSetupAction, NodeSetupPlan, OperationResponse, UpdateStep } from "../types";
import { planNodeSetup, streamNodeSetup, toApiError } from "../services/api";
import { StepProgress, type StepState } from "./environment/StepProgress";
import { useClusterBusyHandler } from "../state/AppInfo";
import { sentence } from "../build";

/** Each action's dialog title and confirm button. */
export const NODE_SETUP_TEXT: Record<NodeSetupAction, { title: string; confirm: string; danger: boolean }> = {
  "mirror-create": { title: "Create the image mirror", confirm: "Create the image mirror", danger: false },
  "mirror-remove": { title: "Remove the image mirror", confirm: "Remove the image mirror", danger: true },
  "pull-secret-add": { title: "Add to the nodes' pull secret", confirm: "Add to the nodes' pull secret", danger: false },
  "pull-secret-remove": { title: "Remove from the nodes' pull secret", confirm: "Remove from the nodes' pull secret", danger: true },
};

const STEPS = [
  { id: "check", title: "Read what is on the cluster" },
  { id: "dry_run", title: "Check the change (server-side dry run)" },
  { id: "apply", title: "Make the change" },
  { id: "verify", title: "Read it back" },
];

/** Why a refused plan falls back to the instructions, as an alert title. */
function refusalTitle(plan: NodeSetupPlan): string {
  switch (plan.refusalCode) {
    case "hosted":
      return "Not on a hosted control plane";
    case "managed":
      return "Not changed on a Red Hat-managed cluster";
    case "forbidden":
      return "The cluster does not allow this change here";
    case "foreign":
      return "Someone else's object is in the way";
    case "foreign_entry":
      return "An entry this tool did not add is already there";
    case "not_owned":
      return "Not added by this tool";
    case "conflict":
      return "It changed meanwhile";
    case "no_token":
    case "quay":
      return "The saved token cannot be used";
    default:
      return "This cannot be done here";
  }
}

type Phase = "planning" | "plan" | "running" | "done";

/**
 * Review, then run, one one-click setup action (the image mirror, the
 * nodes' pull secret, or their removals). Opening it asks the backend for
 * the plan: what is on the cluster now, read with the user's own token,
 * and a server-side dry run of the change. It shows exactly what changes,
 * what the nodes do, and how to undo it; a refusal (RBAC, an admission
 * webhook, a hosted or Red Hat-managed cluster, someone else's object)
 * says why and points at the instructions. Confirm streams the steps.
 */
export const NodeSetupDialog: React.FC<{
  action: NodeSetupAction;
  onClose: () => void;
  /** After a run that may have changed the cluster: refresh the status. */
  onChanged: () => void;
  /**
   * Closes the dialog and runs Check that the nodes can pull: offered when
   * Add finds an entry this tool did not add (if it works, nothing is needed).
   */
  onCheck?: () => void;
}> = ({ action, onClose, onChanged, onCheck }) => {
  const titleId = React.useId();
  const replaceId = React.useId();
  const onBusy = useClusterBusyHandler();
  const [phase, setPhase] = useState<Phase>("planning");
  const [plan, setPlan] = useState<NodeSetupPlan | null>(null);
  const [planError, setPlanError] = useState<string | null>(null);
  const [states, setStates] = useState<Record<string, StepState>>({});
  const [result, setResult] = useState<OperationResponse | null>(null);
  // Add found an entry this tool did not add: the acknowledgement to
  // replace it, unticked; ticking it asks for the plan that replaces it.
  const [replaceOffered, setReplaceOffered] = useState(false);
  const [replace, setReplace] = useState(false);
  const stream = useRef<AbortController | null>(null);
  // The run's outcome in the backend's words (its operation_complete step).
  const finalMessage = useRef<string | null>(null);
  const replacing = !!plan?.replacing;
  const text = replacing ? { title: "Replace the entry in the nodes' pull secret", confirm: "Replace the entry", danger: true } : NODE_SETUP_TEXT[action];

  const loadPlan = useCallback(async (replaceExisting: boolean) => {
    setPhase("planning");
    setPlan(null);
    setPlanError(null);
    try {
      const p = await planNodeSetup(action, replaceExisting);
      if (p.replaceable) setReplaceOffered(true);
      setPlan(p);
    } catch (e) {
      const err = toApiError(e, "Could not work out the change");
      setPlanError(err.message);
      onBusy(err);
    } finally {
      setPhase("plan");
    }
  }, [action, onBusy]);

  useEffect(() => {
    void loadPlan(false);
    return () => stream.current?.abort();
  }, [loadPlan]);

  const onReplace = (checked: boolean) => {
    setReplace(checked);
    void loadPlan(checked);
  };

  const confirm = () => {
    if (!plan) return;
    setPhase("running");
    setStates({});
    setResult(null);
    const onStep = (step: UpdateStep) => {
      if (step.step === "operation_complete") {
        finalMessage.current = step.message;
        return;
      }
      setStates((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
    };
    stream.current = streamNodeSetup(
      plan,
      onStep,
      (success, error, apiError) => {
        setResult({ success, message: error ?? finalMessage.current ?? (success ? "Done." : "It stopped."), logs: [], errorCode: apiError?.errorCode });
        if (apiError) onBusy(apiError);
        setPhase("done");
        onChanged();
      },
      () => {
        setResult({ success: false, message: "The connection was lost. The change may still finish on the server; the Status page shows the result.", logs: [], errorCode: "detached" });
        setPhase("done");
        onChanged();
      },
    );
  };

  const running = phase === "running";
  const ready = phase === "plan" && !!plan?.ready;
  // Someone else's entry: if it works, nothing is needed, so the check is
  // the first thing to try.
  const checkOffered = phase === "plan" && plan?.refusalCode === "foreign_entry" && !!onCheck;

  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={() => !running && onClose()}>
      <ModalHeader title={text.title} labelId={titleId} titleIconVariant={text.danger && ready ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {phase === "planning" && (
            <StackItem>
              <Content component="p"><Spinner size="md" aria-hidden="true" /> Reading the cluster with your permissions and checking the change with a dry run...</Content>
            </StackItem>
          )}
          {planError && (
            <StackItem>
              <Alert component="p" isInline variant="danger" title="Could not work out the change">{sentence(planError)} Nothing was changed.</Alert>
            </StackItem>
          )}
          {plan && phase === "plan" && plan.nothingToDo && (
            <StackItem>
              <Alert component="p" isInline variant="info" title="Nothing to change">{plan.state}</Alert>
            </StackItem>
          )}
          {plan && phase === "plan" && plan.refusal && (
            <StackItem>
              <Alert component="p" isInline variant="warning" title={refusalTitle(plan)}>{plan.refusal}</Alert>
            </StackItem>
          )}
          {replaceOffered && (phase === "plan" || phase === "planning") && (
            <StackItem>
              <Checkbox
                id={replaceId}
                isChecked={replace}
                isDisabled={phase === "planning"}
                onChange={(_e, checked) => onReplace(checked)}
                label="Replace it with the token saved under Pull secret"
                description="The token it has now is not kept, and this tool cannot restore it: only whoever set the entry up has it."
              />
            </StackItem>
          )}
          {plan && (phase === "plan" || running) && plan.ready && (
            <>
              <StackItem>
                <Content component="p">{plan.state}</Content>
              </StackItem>
              <StackItem>
                <Content component="p">This changes the shared cluster, with your own permissions:</Content>
                <List>
                  {(plan.changes ?? []).map((c) => <ListItem key={c}>{c}</ListItem>)}
                </List>
              </StackItem>
              {plan.nodeImpact && (
                <StackItem>
                  <Title headingLevel="h3" size="md">What the nodes do</Title>
                  <Content component="p">{plan.nodeImpact}</Content>
                </StackItem>
              )}
              {plan.pools && plan.pools.length > 0 && (
                <StackItem>
                  <Alert component="p" isInline isPlain variant="warning" title="Some machine config pools will not take it yet">
                    {plan.pools.join(", ")}: their nodes get the change only once the pool is unpaused or no longer degraded.
                  </Alert>
                </StackItem>
              )}
              {plan.undo && (
                <StackItem>
                  <Title headingLevel="h3" size="md">How to undo it</Title>
                  <Content component="p">{plan.undo}</Content>
                </StackItem>
              )}
              {plan.controlPlane === undefined && (
                <StackItem>
                  <Alert component="p" isInline isPlain variant="info" title="The updater could not tell what kind of cluster this is">
                    The API server accepted the change in a dry run, so it is allowed here. If this is a Red Hat-managed cluster, check its documentation before you go on.
                  </Alert>
                </StackItem>
              )}
            </>
          )}
          {(running || phase === "done") && (
            <StackItem>
              <StepProgress idPrefix="node-setup" aria-label={`${text.title}: progress`} steps={STEPS} states={states} running={running} shownElsewhere={result?.message} />
            </StackItem>
          )}
          {phase === "done" && result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={result.success ? "success" : result.errorCode === "detached" ? "info" : "danger"}
                title={result.success ? "Done" : result.errorCode === "detached" ? "The connection was lost" : "It stopped"}>
                {result.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {ready && (
          <Button variant={text.danger ? "danger" : "primary"} onClick={confirm}>{text.confirm}</Button>
        )}
        {running && <Button variant="primary" isLoading isDisabled>{text.confirm}</Button>}
        {checkOffered && <Button variant="primary" onClick={onCheck}>Check that the nodes can pull</Button>}
        <Button variant={ready || running || checkOffered ? "link" : "primary"} onClick={onClose} isDisabled={running}>{ready ? "Cancel" : "Close"}</Button>
      </ModalFooter>
    </Modal>
  );
};
