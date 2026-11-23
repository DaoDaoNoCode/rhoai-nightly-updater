import React from "react";
import { ProgressStep, ProgressStepper } from "@patternfly/react-core";
import type { UpdateStep } from "../../types";

export interface StepState {
  status: UpdateStep["status"];
  message: string;
}

/**
 * The ordered steps of a run as a vertical PatternFly ProgressStepper: one
 * step per part, with its state and latest message. A failed step's message
 * is left out when it is the run's outcome (shownElsewhere): the outcome
 * alert under the steps already says it, and the same text twice reads as
 * two problems. A step that has not started shows its `pending` text, if
 * any (how long it usually takes).
 */
export const StepProgress: React.FC<{
  idPrefix: string;
  "aria-label": string;
  steps: { id: string; title: string; pending?: string }[];
  states: Record<string, StepState>;
  running: boolean;
  shownElsewhere?: string;
}> = ({ idPrefix, "aria-label": ariaLabel, steps, states, running, shownElsewhere }) => (
  <ProgressStepper isVertical aria-label={ariaLabel}>
    {steps.map(({ id, title, pending }) => {
      const s = states[id];
      const current = running && s?.status === "running";
      const variant = !s ? "pending" : s.status === "success" ? "success" : s.status === "failed" ? "danger" : s.status === "skipped" ? "default" : "info";
      const description = !s ? pending : !((s.status === "failed" || s.status === "success") && s.message === shownElsewhere) ? s.message : undefined;
      return (
        <ProgressStep key={id} id={`${idPrefix}-${id}`} titleId={`${idPrefix}-${id}-title`} variant={variant} isCurrent={current}
          aria-label={current ? `${title}: in progress` : undefined} description={description}>
          {title}
        </ProgressStep>
      );
    })}
  </ProgressStepper>
);
