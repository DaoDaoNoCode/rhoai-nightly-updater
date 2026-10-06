import React from "react";
import { Label, type LabelProps } from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import InfoCircleIcon from "@patternfly/react-icons/dist/esm/icons/info-circle-icon";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";

/**
 * The app's status vocabulary (one meaning per color, always with an icon):
 * - success (green): Running, Ready, Succeeded, Passed
 * - progress (blue, in-progress icon): Starting, Installing, Rolling out
 * - info (blue): informational states
 * - warning (orange): Not ready, Terminating, Stale, Downgrade
 * - danger (red): Failed, Error, Missing
 * - neutral (grey, no icon): Not installed, Not deployed, Removed
 */
export type StatusKind = "success" | "progress" | "info" | "warning" | "danger" | "neutral";

const STYLE: Record<StatusKind, { color: LabelProps["color"]; icon: React.ReactNode }> = {
  success: { color: "green", icon: <CheckCircleIcon /> },
  progress: { color: "blue", icon: <InProgressIcon /> },
  info: { color: "blue", icon: <InfoCircleIcon /> },
  warning: { color: "orange", icon: <ExclamationTriangleIcon /> },
  danger: { color: "red", icon: <ExclamationCircleIcon /> },
  neutral: { color: "grey", icon: undefined },
};

interface StatusLabelProps {
  status: StatusKind;
  children: React.ReactNode;
  /** Replaces the default status icon (for example an upgrade arrow or a spinner). */
  icon?: React.ReactNode;
  isCompact?: boolean;
}

/** A status label: the color and icon come from `status`, so no separate icon goes next to it. */
export const StatusLabel: React.FC<StatusLabelProps> = ({ status, children, icon, isCompact = true }) => {
  const style = STYLE[status];
  // The span keeps the label's own width where its parent stretches children (table cells in grid mode).
  return <span><Label isCompact={isCompact} color={style.color} icon={icon ?? style.icon}>{children}</Label></span>;
};

/**
 * A category, not a status (EA/GA, PR #, flavor, "Changed", "Not managed by
 * this tool"): an outline label in a non-status color.
 */
export const TagLabel: React.FC<{
  color?: Extract<LabelProps["color"], "blue" | "purple" | "teal" | "grey">;
  icon?: React.ReactNode;
  children: React.ReactNode;
}> = ({ color = "grey", icon, children }) => (
  <span><Label isCompact variant="outline" color={color} icon={icon}>{children}</Label></span>
);
