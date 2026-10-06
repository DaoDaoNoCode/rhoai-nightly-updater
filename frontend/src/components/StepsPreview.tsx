import React, { useState } from "react";
import { ExpandableSection, ProgressStep, ProgressStepper } from "@patternfly/react-core";
import type { PipelineStepDef } from "../operationSteps";

/**
 * The steps an operation will run, collapsed in a confirmation dialog
 * ("What happens (8 steps)"), drawn like the progress view that follows.
 */
export const StepsPreview: React.FC<{ steps: PipelineStepDef[]; idPrefix: string }> = ({ steps, idPrefix }) => {
  const [expanded, setExpanded] = useState(false);
  return (
    <ExpandableSection
      toggleText={`What happens (${steps.length} steps)`}
      isExpanded={expanded}
      onToggle={(_e, value) => setExpanded(value)}
    >
      <ProgressStepper isVertical isCompact aria-label="Steps of this operation">
        {steps.map((step) => (
          <ProgressStep
            key={step.id}
            id={`${idPrefix}-${step.id}`}
            titleId={`${idPrefix}-${step.id}-title`}
            variant="pending"
            description={step.description}
          >
            {step.label}
          </ProgressStep>
        ))}
      </ProgressStepper>
    </ExpandableSection>
  );
};
