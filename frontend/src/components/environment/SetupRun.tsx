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
import { type GoalRun, type GoalRunKind, shownSteps } from "./useGoalRun";
import type { NextStep } from "./featureState";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";

const RUNNING_NOTE: Record<GoalRunKind, string> = {
  setup: "Each part is created, then checked until it works. You can close this dialog: the setup goes on, and the tab shows each part's state.",
  delete: "You can close this dialog: the deletion goes on, and the tab shows each part's state.",
  change: "You can close this dialog: the change goes on, and the tab shows its state.",
};

const STILL: Record<GoalRunKind, string> = { setup: "Still starting", delete: "Still deleting", change: "Still in progress" };

/** The outcome's variant and title, the same in the dialog and in the toast. */
export function runOutcome(run: GoalRun): { variant: "success" | "info" | "warning" | "danger"; title: string } | null {
  const r = run.result;
  if (!r) return null;
  if (r.success) return { variant: "success", title: r.message };
  if (r.errorCode === "in_progress") return { variant: "info", title: STILL[run.kind] };
  if (r.errorCode === "detached") return { variant: "info", title: "Still running" };
  // pkg/api: the tool's own one-change-per-30-seconds limit; nothing ran.
  if (r.errorCode === "user_rate_limited") return { variant: "warning", title: "Not started: wait before repeating this change" };
  return { variant: "danger", title: `${run.title}: stopped` };
}

/**
 * The progress of any operation on the Environment page (Set up, Delete,
 * Repair, Clean up, a PR deploy): one step per part, its state and message
 * (a set-up part counts only once it works), then the outcome, and the
 * feature's main action once it is ready. Closing it does not stop the run.
 */
export const RunModal: React.FC<{
  run: GoalRun | null;
  isOpen: boolean;
  onClose: () => void;
  /** The feature's next step after a successful run: an Open link becomes the primary button. */
  open: NextStep | null;
  /** What to do next, under a successful outcome. */
  whatNext?: React.ReactNode;
}> = ({ run, isOpen, onClose, open, whatNext }) => {
  const titleId = React.useId();
  if (!run) return null;
  const outcome = runOutcome(run);
  const success = !!run.result?.success;
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={run.title} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <StepProgress idPrefix="goal-run" aria-label={`${run.title}: progress`} steps={shownSteps(run)} states={run.states} running={run.running}
              shownElsewhere={run.result?.message} />
          </StackItem>
          {run.running && (
            <StackItem>
              <Content component="p">{RUNNING_NOTE[run.kind]}</Content>
            </StackItem>
          )}
          {outcome && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={outcome.variant} title={outcome.title}>
                {success ? whatNext : run.result?.message}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {success && open?.kind === "open" && (
          <Button variant="primary" component="a" href={open.href} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end">{open.label}</Button>
        )}
        <Button variant={success && open?.kind === "open" ? "link" : "primary"} onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
