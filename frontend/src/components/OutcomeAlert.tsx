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
  const message = result.success ? undefined : result.message;
  // Alert shows its description only when it has children: a lone message
  // that is empty (a success) leaves none.
  if (!logs) return <Alert {...props}>{message}</Alert>;
  return (
    <Alert {...props}>
      {message}
      {children}
      {(result.logs?.length ?? 0) > 0 && <TechnicalDetails text={result.logs!} />}
    </Alert>
  );
};
