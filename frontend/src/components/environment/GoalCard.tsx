import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  Button,
  Card,
  CardBody,
  CardFooter,
  CardHeader,
  CardTitle,
  Content,
  Form,
  FormGroup,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  ProgressStep,
  ProgressStepper,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import { TooltipButton } from "../TooltipButton";
import { ExternalLink } from "./resourceRow";
import { envStatusText, type EnvStatus } from "./resourceStatus";
import { ProjectSelect, type ProjectOption } from "./ProjectSelect";
import { StepProgress } from "./StepProgress";
import type { GoalRun } from "./useGoalRun";

/** One link of a goal's chain: a resource of the Cluster or the Projects tab. */
export interface GoalStep {
  id: string;
  title: string;
  /** The title in a sentence ("the storage", "MLflow"). */
  noun: string;
  /** Where it lives: "Cluster" or "Project <name>". */
  scope: string;
  status: EnvStatus;
  /** It can be set up as part of the goal (it is not set up yet). */
  canSetUp: boolean;
  /** Shows its card and details. */
  show: () => void;
}

/** Something a user wants to do, and the resources it needs, in order. */
export interface Goal {
  id: string;
  title: string;
  description: string;
  /** It needs a project: the page's selected one. */
  projectScoped: boolean;
  steps: GoalStep[];
  /** Where to go once every step is ready. */
  open?: { label: string; href: string };
}

/** A step works when it is ready, whoever created it. */
export const stepReady = (s: GoalStep) => s.status === "ready";
const stepBlocked = (s: GoalStep) => !stepReady(s) && !s.canSetUp && s.status !== "setting-up";

const VARIANT: Record<EnvStatus, "success" | "info" | "pending" | "warning" | "danger"> = {
  ready: "success",
  "setting-up": "info",
  deleting: "info",
  "not-set-up": "pending",
  "needs-attention": "warning",
  error: "danger",
  "not-managed": "warning",
};

/**
 * A goal card: the chain of resources the goal needs with their status,
 * how many are ready, and one action: set up what is missing, show the
 * step that needs attention, or open the result.
 */
export const GoalCard: React.FC<{
  goal: Goal;
  /** The page's projects, for a project-scoped goal without a selected project. */
  projects: ProjectOption[];
  project: string | null;
  onProject: (project: string) => void;
  baseReason: string | null;
  onSetUp: () => void;
}> = ({ goal, projects, project, onProject, baseReason, onSetUp }) => {
  const headingId = `goal-${goal.id}-title`;
  const ready = goal.steps.filter(stepReady).length;
  const total = goal.steps.length;
  const blocked = goal.steps.find(stepBlocked);
  const settingUp = goal.steps.find((s) => s.status === "setting-up" || s.status === "deleting");
  const needsProject = goal.projectScoped && !project;
  let action: React.ReactNode;
  if (!needsProject && ready === total) {
    action = goal.open ? <ExternalLink href={goal.open.href}>{goal.open.label}</ExternalLink> : null;
  } else if (!needsProject && blocked) {
    action = <Button variant="link" onClick={blocked.show}>{`Show ${blocked.noun}`}</Button>;
  } else {
    const reason = baseReason
      ?? (needsProject ? "Choose a project first." : null)
      ?? (settingUp ? `${settingUp.title} is still ${settingUp.status === "deleting" ? "being deleted" : "setting up"}. Wait until it is ready.` : null);
    action = <TooltipButton variant="primary" onClick={onSetUp} disabledReason={reason}>{ready > 0 ? "Set up the rest" : "Set up"}</TooltipButton>;
  }
  return (
    <Card isFullHeight component="section" aria-labelledby={headingId}>
      <CardHeader>
        <CardTitle><Title headingLevel="h3" size="lg" id={headingId}>{goal.title}</Title></CardTitle>
      </CardHeader>
      <CardBody>
        <Stack hasGutter>
          <StackItem><Content component="p">{goal.description}</Content></StackItem>
          {needsProject && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label="Project" fieldId={`goal-${goal.id}-project`}>
                  <ProjectSelect id={`goal-${goal.id}-project`} projects={projects} selected={null} onSelect={onProject} />
                </FormGroup>
              </Form>
            </StackItem>
          )}
          <StackItem>
            <ProgressStepper isVertical aria-label={`${goal.title}: what it needs`}>
              {goal.steps.map((s) => (
                <ProgressStep key={s.id} id={`goal-${goal.id}-${s.id}`} titleId={`goal-${goal.id}-${s.id}-title`} variant={VARIANT[s.status]}
                  icon={s.status === "setting-up" || s.status === "deleting" ? <Spinner size="sm" aria-hidden /> : undefined}
                  description={`${s.scope}: ${envStatusText(s.status)}`}>
                  {s.title}
                </ProgressStep>
              ))}
            </ProgressStepper>
          </StackItem>
          <StackItem><Content component="small">{ready} of {total} ready</Content></StackItem>
        </Stack>
      </CardBody>
      {action && (
        <CardFooter>
          <ActionList>
            <ActionListGroup><ActionListItem>{action}</ActionListItem></ActionListGroup>
          </ActionList>
        </CardFooter>
      )}
    </Card>
  );
};

/**
 * The progress of a goal's setup: one step per setup, its state and
 * message, then the outcome. Closing it does not stop the run.
 */
export const GoalRunModal: React.FC<{ run: GoalRun | null; isOpen: boolean; onClose: () => void }> = ({ run, isOpen, onClose }) => {
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
