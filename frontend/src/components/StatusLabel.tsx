import React from "react";
import { Label, type LabelProps } from "@patternfly/react-core";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";

/**
 * The app's status vocabulary (one meaning per color):
 * - success: Running, Ready, Succeeded, Passed
 * - progress: Starting, Installing, Rolling out (blue, in-progress icon)
 * - info: informational states (blue, info icon)
 * - warning: Not ready, Terminating, Stale, Downgrade
 * - danger: Failed, Error, Missing
 * - neutral: Not installed, Not deployed, Removed (grey, no icon)
 */
export type StatusKind = "success" | "progress" | "info" | "warning" | "danger" | "neutral";

interface StatusLabelProps {
  status: StatusKind;
  children: React.ReactNode;
  /** Replaces the default status icon (for example an upgrade arrow). */
  icon?: React.ReactNode;
  /** Compact everywhere except where the label is the main subject of a card header. */
  isCompact?: boolean;
}

/**
 * A status as a PatternFly status label: the color and icon come from
 * `status`, so no separate icon goes next to it.
 */
export const StatusLabel: React.FC<StatusLabelProps> = ({ status, children, icon, isCompact = true }) => {
  if (status === "neutral") return <Label isCompact={isCompact} color="grey">{children}</Label>;
  if (status === "progress") {
    return <Label isCompact={isCompact} status="info" icon={icon ?? <InProgressIcon />}>{children}</Label>;
  }
  return <Label isCompact={isCompact} status={status} icon={icon}>{children}</Label>;
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
  <Label isCompact variant="outline" color={color} icon={icon}>{children}</Label>
);
