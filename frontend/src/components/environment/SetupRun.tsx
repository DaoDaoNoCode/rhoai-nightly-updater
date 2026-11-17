import React from "react";
import {
  Alert,
  Button,
  Content,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { StepProgress } from "./StepProgress";
import type { GoalRun } from "./useGoalRun";
import type { NextStep } from "./featureState";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";

/**
 * The progress of a feature's Set up: one step per part, its state and
 * message (a part counts only once it works), then the outcome, and the
 * feature's main action once it is ready. Closing it does not stop the run.
 */
export const SetupRunModal: React.FC<{
  run: GoalRun | null;
  isOpen: boolean;
  onClose: () => void;
  /** The feature's next step after the run, from its state function: an Open link becomes the primary button. */
  open: NextStep | null;
}> = ({ run, isOpen, onClose, open }) => {
  const titleId = React.useId();
  if (!run) return null;
  const starting = run.result?.errorCode === "in_progress";
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={run.title} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <StepProgress idPrefix="goal-run" aria-label={`${run.title}: progress`} steps={run.steps} states={run.states} running={run.running}
              shownElsewhere={run.result && !run.result.success ? run.result.message : undefined} />
          </StackItem>
          {run.running && (
            <StackItem>
              <Content component="p">Each part is created, then checked until it works. You can close this dialog: the setup goes on, and the tab shows each part&apos;s state.</Content>
            </StackItem>
          )}
          {run.result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={run.result.success ? "success" : starting ? "info" : "danger"}
                title={run.result.success ? run.result.message : starting ? "Still starting" : "Stopped"}>
                {run.result.success ? undefined : run.result.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {open?.kind === "open" && (
          <Button variant="primary" component="a" href={open.href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{open.label}</Button>
        )}
        <Button variant="link" onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
