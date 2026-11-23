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
import { Link } from "react-router-dom";
import { getResourcesStatus, streamCleanup, type CleanupRequest } from "../../services/api";
import type { RecipeRun, ResourcesStatus } from "../../types";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { cleanupSteps } from "../environment/CleanupModal";
import { TooltipButton } from "../TooltipButton";
import { cleanupParts, cleanupRequest, leftInPlace } from "./recipeForm";

/** The clean-up's refusal of a recipe project that still holds what the tool did not create (pkg/cluster recipe.ErrorCodeConfirmContents). */
const CONFIRM_CONTENTS = "confirm_project_contents";
const PROJECT_STEP = "recipe-project:";

interface Outcome {
  success: boolean;
  message: string;
  errorCode?: string;
}

/**
 * Clean up what a recipe set up, as one clean-up operation (the
 * Environment page's, POST /api/resources/cleanup/stream). It sends the
 * backend's request (`ready`: the projects recipes created, with
 * everything in them, and the DataScienceCluster components a run turned
 * on), less what the user leaves out. For a run (`run`), what it leaves in
 * place (the S3 storage, MLflow, a pipeline server in a project the recipe
 * did not create) is listed from the live Environment status, for Clean up
 * everything on the Environment page. A project that holds what this tool
 * did not create (a workbench, a claim) is refused before anything is
 * deleted; the dialog lists it, and deletes it with the project only once
 * the user confirms. The outcome says what the clean-up did, in the
 * backend's words, and the progress keeps the parts that were sent, also
 * once the run has nothing left to clean up.
 */
export const RecipeCleanupModal: React.FC<{
  /** The run whose parts these are; null for a project alone (no "Left in place"). */
  run: RecipeRun | null;
  title: string;
  /** The backend's clean-up request for what is the recipe's (GET /api/recipes/run cleanup, or one project). */
  ready: CleanupRequest | undefined;
  isOpen: boolean;
  /** Why a change cannot start now, or null. */
  blocker: string | null;
  onClose: () => void;
  /** The clean-up ended (the run and the operation state may have changed). */
  onDone: () => void;
  onClusterBusy: (res: { errorCode?: string }) => void;
}> = ({ run, title, ready, isOpen, blocker, onClose, onDone, onClusterBusy }) => {
  const titleId = React.useId();
  // What is offered stays as it was when the dialog opened: polling (and the
  // clean-up itself) changes `ready`, never the user's choices.
  const [offered] = useState(ready);
  const parts = cleanupParts(offered);
  const [chosen, setChosen] = useState<ReadonlySet<string>>(() => new Set(parts.map((p) => p.id)));
  // What stays, from the live status (null until it is read; false when it cannot be).
  const [resources, setResources] = useState<ResourcesStatus | false | null>(null);
  const [running, setRunning] = useState(false);
  const [states, setStates] = useState<Record<string, StepState>>({});
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // The request the last clean-up sent: its steps are what the progress shows.
  const [sent, setSent] = useState<CleanupRequest | null>(null);
  // Per project, what it holds that the user must confirm, from the refusal's step detail (one per line).
  const [contents, setContents] = useState<Record<string, string[]>>({});
  const [confirmed, setConfirmed] = useState<ReadonlySet<string>>(new Set());
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  useEffect(() => {
    if (!run) return;
    const ticket = new AbortController();
    getResourcesStatus(null, ticket.signal).then(setResources, () => !ticket.signal.aborted && setResources(false));
    return () => ticket.abort();
  }, [run]);

  const confirmedContents = Object.fromEntries(Object.entries(contents).filter(([p]) => confirmed.has(p)));
  const req = cleanupRequest(offered, chosen, confirmedContents);
  const stays = run && resources ? leftInPlace(resources, run, req) : [];
  const steps = cleanupSteps(sent ?? req);
  const started = running || outcome !== null;
  const asking = Object.keys(contents).filter((p) => chosen.has(`${PROJECT_STEP}${p}`));

  const start = () => {
    setRunning(true);
    setOutcome(null);
    setStates({});
    setSent(req);
    const refusals: Record<string, string[]> = {};
    let said = "";
    controller.current = streamCleanup(
      req,
      (step) => {
        if (step.step === "operation_complete") said = step.message;
        setStates((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
        if (step.step.startsWith(PROJECT_STEP) && step.errorCode === CONFIRM_CONTENTS) {
          refusals[step.step.slice(PROJECT_STEP.length)] = (step.detail ?? "").split("\n").filter(Boolean);
        }
      },
      (success, error, apiError) => {
        setRunning(false);
        const code = apiError?.errorCode;
        if (code) onClusterBusy({ errorCode: code });
        if (code === CONFIRM_CONTENTS && Object.keys(refusals).length > 0) {
          setContents(refusals);
          setConfirmed(new Set());
        }
        setOutcome({ success, message: success ? said || "Cleaned up." : error ?? "The clean-up failed.", errorCode: code });
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
  const toggleConfirmed = (project: string, on: boolean) => setConfirmed((c) => {
    const next = new Set(c);
    if (on) next.add(project); else next.delete(project);
    return next;
  });

  const unconfirmed = asking.filter((p) => !confirmed.has(p));
  const reason = blocker ?? (cleanupSteps(req).length === 0 ? "Choose what to delete." : unconfirmed.length > 0 ? "Confirm that what the project holds is deleted with it." : null);
  const done = outcome?.success === true;
  const refused = !!outcome && !outcome.success && asking.length > 0;

  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={() => !running && onClose()}>
      <ModalHeader title={title} labelId={titleId} titleIconVariant={started ? undefined : "warning"} />
      <ModalBody>
        <Stack hasGutter>
          {!started || refused ? (
            <>
              <StackItem>
                <Content component="p">
                  Deletes what you choose, as one operation, in this order. RHOAI and the DataScienceCluster stay.
                </Content>
              </StackItem>
              <StackItem>
                <Form onSubmit={(e) => e.preventDefault()}>
                  <FormGroup role="group" isStack fieldId="recipe-cleanup-parts" label="Delete">
                    {parts.map((p) => (
                      <Checkbox key={p.id} id={`recipe-cleanup-${p.id}`} label={p.title} isChecked={chosen.has(p.id)} onChange={(_e, v) => toggle(p.id, v)}
                        description={p.id === "components" ? "Each goes back to what it was, unless something on the cluster uses it."
                          : "Removes everything in the project, including its pipeline server."} />
                    ))}
                  </FormGroup>
                </Form>
              </StackItem>
              {(req.recipeProjects ?? []).map((project) => (
                <StackItem key={`danger-${project}`}>
                  <Alert variant="danger" isInline component="p" title="Data is deleted and cannot be recovered">
                    Deleting project {project} deletes everything in it. It is deleted only while it is still the one the recipe created.
                  </Alert>
                </StackItem>
              ))}
              {run && (stays.length > 0 || resources === false) && (
                <StackItem>
                  <Alert variant="info" isInline title="Left in place">
                    <Stack hasGutter>
                      {stays.length > 0 ? (
                        <>
                          <StackItem>This clean-up does not delete these:</StackItem>
                          <StackItem>
                            <List>{stays.map((s) => <ListItem key={s}>{s}</ListItem>)}</List>
                          </StackItem>
                        </>
                      ) : (
                        <StackItem>
                          This clean-up does not delete the S3 storage, MLflow or a pipeline server in a project the recipe did not create.
                          Their status could not be read.
                        </StackItem>
                      )}
                      <StackItem>
                        To delete them, use Clean up everything on the <Link to="/environment">Environment page</Link>.
                      </StackItem>
                    </Stack>
                  </Alert>
                </StackItem>
              )}
              {asking.map((project) => (
                <StackItem key={`ask-${project}`}>
                  <Alert variant="warning" isInline title={`Project ${project} holds what this tool did not create`}>
                    <Stack hasGutter>
                      <StackItem>Nothing was deleted yet. Deleting the project deletes these too:</StackItem>
                      <StackItem>
                        <List>{contents[project].map((c) => <ListItem key={c}>{c}</ListItem>)}</List>
                      </StackItem>
                      <StackItem>
                        <Checkbox id={`recipe-cleanup-confirm-${project}`} label={`Delete ${contents[project].length === 1 ? "it" : "them"} with the project`}
                          isChecked={confirmed.has(project)} onChange={(_e, v) => toggleConfirmed(project, v)} />
                      </StackItem>
                    </Stack>
                  </Alert>
                </StackItem>
              ))}
            </>
          ) : (
            <StackItem>
              <StepProgress idPrefix="recipe-cleanup" aria-label="Clean up: progress" steps={steps} states={states} running={running}
                shownElsewhere={outcome?.message} />
            </StackItem>
          )}
          {outcome && !refused && (
            <StackItem>
              <Alert variant={outcome.success ? "success" : outcome.errorCode === "detached" ? "info" : "danger"} isInline component="p" isLiveRegion
                title={outcome.success ? "Cleaned up" : outcome.errorCode === "detached" ? "Still running" : "The clean-up stopped"}>
                {outcome.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {done || (outcome && !refused) ? (
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
