import React from "react";
import { Button, Tooltip, type ButtonProps } from "@patternfly/react-core";

export interface TooltipButtonProps extends ButtonProps {
  /**
   * Why the button can't be used right now. When set, the button is
   * aria-disabled and shows this text on hover and keyboard focus.
   */
  disabledReason?: string | null;
}

/**
 * A Button that explains why it is disabled. Natively disabled buttons can't
 * receive focus or hover, so their tooltips never show. PatternFly's
 * aria-disabled buttons stay focusable and support tooltips, and PF blocks
 * their click and key-press handlers
 * (https://www.patternfly.org/components/button, "Aria-disabled examples").
 */
export const TooltipButton: React.FC<TooltipButtonProps> = ({ disabledReason, isDisabled, ...props }) => {
  if (disabledReason) {
    return (
      <Tooltip content={disabledReason}>
        <Button {...props} isAriaDisabled />
      </Tooltip>
    );
  }
  return <Button {...props} isDisabled={isDisabled} />;
};

export const NO_PERMISSION_REASON = "You don't have permission to change this cluster. Contact a cluster admin.";
export const OPERATION_RUNNING_REASON = "Another operation is in progress. Wait for it to finish.";
