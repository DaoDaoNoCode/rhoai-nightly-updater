import React, { useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  Form,
  FormGroup,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { streamCleanup } from "../../services/api";
import type { RecipeRun } from "../../types";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { cleanupSteps } from "../environment/CleanupModal";
import { TooltipButton } from "../TooltipButton";
import { cleanupParts, cleanupRequest } from "./recipeForm";

/** The clean-up's refusal of a recipe project that still holds what the tool did not create (pkg/cluster recipe.ErrorCodeConfirmContents). */
const CONFIRM_CONTENTS = "confirm_project_contents";

interface Outcome {
  success: boolean;
  message: string;
  errorCode?: string;
}

/**
 * Clean up what a recipe run set up, as one clean-up operation (the
 * Environment page's, POST /api/resources/cleanup/stream): the parts its
 * steps set up are chosen, the ones that were there before are offered,
 * and the project the recipe created goes last. A project that holds what
 * this tool did not create (a workbench, a claim) is refused before
 * anything is deleted; the dialog lists it, and deletes it with the
 * project only once the user confirms.
 */
export const RecipeCleanupModal: React.FC<{
  run: RecipeRun;
  isOpen: boolean;
  /** Why a change cannot start now, or null. */
  blocker: string | null;
  onClose: () => void;
  /** The clean-up ended (the run and the operation state may have changed). */
  onDone: () => void;
  onClusterBusy: (res: { errorCode?: string }) => void;
}> = ({ run, isOpen, blocker, onClose, onDone, onClusterBusy }) => {
  const titleId = React.useId();
  const parts = cleanupParts(run);
  const [chosen, setChosen] = useState<ReadonlySet<string>>(() => new Set(parts.filter((p) => p.setUpByRun).map((p) => p.id)));
  const [running, setRunning] = useState(false);
  const [states, setStates] = useState<Record<string, StepState>>({});
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // What the project holds that the user must confirm, from the refusal's step detail (one per line).
  const [contents, setContents] = useState<string[]>([]);
  const [confirmed, setConfirmed] = useState(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);

  const project = run.project?.name ?? "";
  const projectId = `recipe-project:${project}`;
  const req = cleanupRequest(run, chosen, confirmed ? contents : []);
  const steps = cleanupSteps(req);
  const started = running || outcome !== null;

  const start = () => {
    setRunning(true);
    setOutcome(null);
    setStates({});
    let refusal: string[] = [];
    controller.current = streamCleanup(
      req,
      (step) => {
        setStates((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
        if (step.step === projectId && step.errorCode === CONFIRM_CONTENTS) refusal = (step.detail ?? "").split("\n").filter(Boolean);
      },
      (success, error, apiError) => {
        setRunning(false);
        const code = apiError?.errorCode;
        if (code) onClusterBusy({ errorCode: code });
        if (code === CONFIRM_CONTENTS && refusal.length > 0) {
          setContents(refusal);
          setConfirmed(false);
        }
        setOutcome({ success, message: success ? "Cleaned up." : error ?? "The clean-up failed.", errorCode: code });
        onDone();
      },
      () => {
        setRunning(false);
        setOutcome({ success: false, message: "The connection to the clean-up was lost. It goes on on the server; the Environment page shows what is left.", errorCode: "detached" });
        onDone();
      },
    );
  };

  const toggle = (id: string, on: boolean) => setChosen((c) => {
    const next = new Set(c);
    if (on) next.add(id); else next.delete(id);
    return next;
  });

  const needsConfirm = contents.length > 0 && chosen.has(projectId);
  const reason = blocker ?? (steps.length === 0 ? "Choose what to delete." : needsConfirm && !confirmed ? "Confirm that what the project holds is deleted with it." : null);
  const done = outcome?.success === true;

  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={() => !running && onClose()}>
      <ModalHeader title={`Clean up what recipe ${run.recipe.metadata.name} set up?`} labelId={titleId} titleIconVariant={started ? undefined : "warning"} />
      <ModalBody>
        <Stack hasGutter>
          {!started || (outcome && !outcome.success && contents.length > 0) ? (
            <>
              <StackItem>
                <Content component="p">
                  Deletes what you choose, as one operation, in this order. RHOAI and the DataScienceCluster stay, and so do the DataScienceCluster
                  components the run turned on: Clean up everything on the Environment page turns them back.
                </Content>
              </StackItem>
              <StackItem>
                <Form onSubmit={(e) => e.preventDefault()}>
                  <FormGroup role="group" isStack fieldId="recipe-cleanup-parts" label="Delete">
                    {parts.map((p) => (
                      <Checkbox key={p.id} id={`recipe-cleanup-${p.id}`} label={p.title} isChecked={chosen.has(p.id)} onChange={(_e, v) => toggle(p.id, v)}
                        description={p.setUpByRun ? undefined : "It was set up before this run: someone may use it."} />
                    ))}
                  </FormGroup>
                </Form>
              </StackItem>
              {chosen.has(projectId) && (
                <StackItem>
                  <Alert variant="danger" isInline component="p" title="Data is deleted and cannot be recovered">
                    Deleting project {project} deletes everything in it. It is deleted only while it is still the one the recipe created.
                  </Alert>
                </StackItem>
              )}
              {needsConfirm && (
                <StackItem>
                  <Alert variant="warning" isInline title={`Project ${project} holds what this tool did not create`}>
                    <Stack hasGutter>
                      <StackItem>Nothing was deleted yet. Deleting the project deletes these too:</StackItem>
                      <StackItem>
                        <List>{contents.map((c) => <ListItem key={c}>{c}</ListItem>)}</List>
                      </StackItem>
                      <StackItem>
                        <Checkbox id="recipe-cleanup-confirm" label={`Delete ${contents.length === 1 ? "it" : "them"} with the project`} isChecked={confirmed}
                          onChange={(_e, v) => setConfirmed(v)} />
                      </StackItem>
                    </Stack>
                  </Alert>
                </StackItem>
              )}
            </>
          ) : (
            <StackItem>
              <StepProgress idPrefix="recipe-cleanup" aria-label="Clean up: progress" steps={steps} states={states} running={running}
                shownElsewhere={outcome?.message} />
            </StackItem>
          )}
          {outcome && !(outcome.errorCode === CONFIRM_CONTENTS && contents.length > 0) && (
            <StackItem>
              <Alert variant={outcome.success ? "success" : outcome.errorCode === "detached" ? "info" : "danger"} isInline component="p" isLiveRegion
                title={outcome.success ? "Cleaned up" : outcome.errorCode === "detached" ? "Still running" : "The clean-up stopped"}>
                {outcome.success ? `Deleted ${steps.length === 1 ? "it" : `the ${steps.length} parts`}.` : outcome.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {done || (outcome && outcome.errorCode !== CONFIRM_CONTENTS) ? (
          <Button variant="primary" onClick={onClose}>Close</Button>
        ) : (
          <>
            <TooltipButton variant="danger" onClick={start} isLoading={running} isDisabled={running} disabledReason={running ? null : reason}>
              Clean up
            </TooltipButton>
            <Button variant="link" onClick={onClose} isDisabled={running}>Cancel</Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};
