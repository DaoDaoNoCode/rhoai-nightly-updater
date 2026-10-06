import React, { useEffect, useState } from "react";
import { Timestamp, TimestampTooltipVariant, Tooltip } from "@patternfly/react-core";
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
  /**
   * "inherit" for a time inside a sentence or a build line: it takes the
   * surrounding font size, weight and color. The default is PF's small,
   * subtle timestamp, for table cells and metadata.
   */
  size?: "sm" | "inherit";
}

/**
 * The one date format of the app: a relative time ("3h ago"), with the
 * absolute local date and time in a tooltip that is also reachable by
 * keyboard.
 */
export const RelativeTime: React.FC<RelativeTimeProps> = ({ date, prefix = "", size = "sm" }) => {
  useTick(30_000);
  const d = typeof date === "string" ? new Date(date) : date;
  if (Number.isNaN(d.getTime())) return <span className="pf-v6-u-text-color-subtle">Unknown</span>;
  const text = `${prefix}${formatRelativeTime(d.toISOString())}`;
  if (size === "inherit") {
    // A plain <time> inherits the sentence's font; PF Timestamp sets its own size.
    return (
      <>
        <Tooltip content={d.toLocaleString()}>
          <time dateTime={d.toISOString()} className="pf-v6-u-text-nowrap">{text}</time>
        </Tooltip>
        <span className="pf-v6-screen-reader"> ({d.toLocaleString()})</span>
      </>
    );
  }
  return (
    <Timestamp className="pf-v6-u-font-size-sm" date={d} tooltip={{ variant: TimestampTooltipVariant.custom, content: d.toLocaleString() }}>
      {text}
    </Timestamp>
  );
};

/**
 * A value the build did not record (no commit, date or version label): a
 * muted em dash, named for screen readers and explained in a tooltip.
 */
export const NotRecorded: React.FC<{ what?: string }> = ({ what = "Not recorded in the image labels" }) => (
  <>
    {/* Not focusable: a table can hold dozens of these; screen readers get the text below. */}
    <Tooltip content={what}>
      <span className="pf-v6-u-text-color-subtle" aria-hidden="true">—</span>
    </Tooltip>
    <span className="pf-v6-screen-reader">{what}</span>
  </>
);
