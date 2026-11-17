import React, { useRef } from "react";
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
  // triggerRef, not children: wrapping the button makes PatternFly add a
  // display: contents <div> around it, so in a layout that styles its direct
  // children (a Form ActionGroup gives them margins) the button lost its
  // margins and was stretched and clipped.
  const ref = useRef<HTMLButtonElement>(null);
  if (disabledReason) {
    return (
      <>
        <Button {...props} ref={ref} isAriaDisabled />
        <Tooltip content={disabledReason} triggerRef={ref} />
      </>
    );
  }
  return <Button {...props} isDisabled={isDisabled} />;
};

export const NO_PERMISSION_REASON = "You don't have permission to change this cluster. Contact a cluster admin.";
export const OPERATION_RUNNING_REASON = "Another operation is in progress. Wait for it to finish.";
