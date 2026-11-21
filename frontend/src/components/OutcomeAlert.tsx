import React from "react";
import { Alert, AlertActionCloseButton } from "@patternfly/react-core";
import type { OperationResponse } from "../types";
import { outcomeTitle, outcomeVariant } from "../outcomes";
import { TechnicalDetails } from "./LongText";

interface OutcomeAlertProps {
  result: OperationResponse;
  /** The title of a failure (outcomeTitle's failedTitle). */
  failedTitle: string;
  /** In a table cell or a dense row. */
  isPlain?: boolean;
  /** Offers a close button. */
  onClose?: () => void;
  /** Shows the result's logs under the message (Show details), with `children` between them. */
  logs?: boolean;
  /** Words after the message (only with `logs`). */
  children?: React.ReactNode;
}

/**
 * The outcome of a change the user asked for (an OperationResponse): its
 * variant and title from outcomes.ts, the message of a failure, and
 * optionally its logs. Announced to screen readers.
 */
export const OutcomeAlert: React.FC<OutcomeAlertProps> = ({ result, failedTitle, isPlain, onClose, logs, children }) => {
  const props = {
    component: "p" as const,
    variant: outcomeVariant(result),
    title: outcomeTitle(result, failedTitle),
    isInline: true,
    isPlain,
    isLiveRegion: true,
    actionClose: onClose ? <AlertActionCloseButton onClose={onClose} /> : undefined,
  };
  const message = result.success ? undefined : result.message || undefined;
  const words = logs && React.Children.toArray(children).length > 0 ? children : undefined;
  // TechnicalDetails renders nothing for blank logs.
  const details = logs && (result.logs ?? []).join("\n").trim() ? <TechnicalDetails text={result.logs!} /> : undefined;
  // Alert renders its description whenever its children are truthy, and an
  // array of empty parts is: a success with nothing under its title would
  // get an empty description and space under the title.
  const body = message || words || details ? <>{message}{words}{details}</> : undefined;
  return <Alert {...props}>{body}</Alert>;
};
