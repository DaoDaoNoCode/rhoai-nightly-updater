import React, { useEffect, useState } from "react";
import { Timestamp, TimestampTooltipVariant } from "@patternfly/react-core";
import { formatRelativeTime } from "../utils";

/** Re-renders every `ms` so relative times stay current. */
function useTick(ms: number): void {
  const [, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((n) => n + 1), ms);
    return () => clearInterval(id);
  }, [ms]);
}

interface RelativeTimeProps {
  /** ISO string or Date. */
  date: string | Date;
  /** Text before the relative time ("Updated ", "built "). */
  prefix?: string;
  /** "inherit" matches the surrounding text (inside a sentence or a build line); default is PF's small subtle timestamp. */
  size?: "sm" | "inherit";
}

/**
 * The one date format of the app: a relative time ("3h ago"), with the
 * absolute local date and time in a tooltip that is also reachable by
 * keyboard (PatternFly Timestamp).
 */
export const RelativeTime: React.FC<RelativeTimeProps> = ({ date, prefix = "", size = "sm" }) => {
  useTick(30_000);
  const d = typeof date === "string" ? new Date(date) : date;
  if (Number.isNaN(d.getTime())) return <span className="pf-v6-u-text-color-subtle">Unknown</span>;
  return (
    <Timestamp className={size === "inherit" ? "pf-v6-u-font-size-md pf-v6-u-text-color-regular" : undefined} date={d} tooltip={{ variant: TimestampTooltipVariant.custom, content: d.toLocaleString() }}>
      {prefix}{formatRelativeTime(d.toISOString())}
    </Timestamp>
  );
};
