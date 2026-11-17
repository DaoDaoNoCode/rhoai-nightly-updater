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

/**
 * The progress of a feature's Set up: one step per setup, its state and
 * message, then the outcome. Closing it does not stop the run.
 */
export const SetupRunModal: React.FC<{ run: GoalRun | null; isOpen: boolean; onClose: () => void }> = ({ run, isOpen, onClose }) => {
  const titleId = React.useId();
  if (!run) return null;
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={run.title} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <StepProgress idPrefix="goal-run" aria-label={`${run.title}: progress`} steps={run.steps} states={run.states} running={run.running}
              shownElsewhere={run.result && !run.result.success ? run.result.message : undefined} />
          </StackItem>
          {run.running && <StackItem><Content component="p">Each part waits until it is ready. You can close this dialog: the setup goes on, and the cards show its progress.</Content></StackItem>}
          {run.result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={run.result.success ? "success" : "danger"} title={run.result.success ? "Set up" : "Stopped"}>
                {run.result.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="link" onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
