import React, { useState } from "react";
import {
  Button,
  Content,
  DataList,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  Flex,
  FlexItem,
  Icon,
  ToggleGroup,
  ToggleGroupItem,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import type { ActivityEntry } from "../types";
import { formatRelativeTime } from "../utils";

/** Fallback category for entries from a backend that predates read-time labels. */
const LEGACY_CATEGORIES: Record<string, string> = {
  update: "operator",
  refresh: "operator",
  reinstall: "operator",
  rollback: "operator",
  "create-pull-secret": "setup",
  "create-dsc": "setup",
  "repair-dsc": "setup",
};

export function activityCategory(entry: ActivityEntry): string {
  if (entry.category) return entry.category;
  if (LEGACY_CATEGORIES[entry.action]) return LEGACY_CATEGORIES[entry.action];
  if (/dashboard|minio|mlflow|pipeline|deploy-pr/.test(entry.action)) return "dashboard-dev";
  return "other";
}

export function activityLabel(entry: ActivityEntry): string {
  if (entry.label) return entry.label;
  const s = entry.action.replace(/-/g, " ");
  return s.charAt(0).toUpperCase() + s.slice(1);
}

const FILTERS: { id: string; label: string }[] = [
  { id: "operator", label: "Operator" },
  { id: "dashboard-dev", label: "Dashboard Dev" },
  { id: "setup", label: "Setup" },
  { id: "diagnostics", label: "Diagnostics" },
  { id: "all", label: "All" },
];

const PAGE_SIZE = 6;

interface ActivityLogProps {
  activity?: ActivityEntry[];
  /** Category shown first (default: operator changes). */
  defaultCategory?: string;
}

/**
 * Who changed what on the cluster, newest first. The backend labels each
 * entry, groups it by category (it keeps 50 operator entries apart from
 * the noisier Dashboard Dev ones) and names the build of operator changes.
 */
export const ActivityLog: React.FC<ActivityLogProps> = React.memo(({ activity, defaultCategory = "operator" }) => {
  const [filter, setFilter] = useState(defaultCategory);
  const [expanded, setExpanded] = useState(false);
  const all = [...(activity ?? [])].reverse();
  const counts = new Map<string, number>();
  for (const e of all) counts.set(activityCategory(e), (counts.get(activityCategory(e)) ?? 0) + 1);
  const entries = filter === "all" ? all : all.filter((e) => activityCategory(e) === filter);
  const shown = expanded ? entries : entries.slice(0, PAGE_SIZE);

  return (
    <Flex direction={{ default: "column" }} gap={{ default: "gapMd" }}>
      <FlexItem>
        <ToggleGroup isCompact aria-label="Filter activity by type">
          {FILTERS.filter((f) => f.id === "all" || f.id === filter || (counts.get(f.id) ?? 0) > 0).map((f) => (
            <ToggleGroupItem
              key={f.id}
              text={f.id === "all" ? `${f.label} (${all.length})` : `${f.label} (${counts.get(f.id) ?? 0})`}
              buttonId={`activity-filter-${f.id}`}
              isSelected={filter === f.id}
              onChange={() => { setFilter(f.id); setExpanded(false); }}
            />
          ))}
        </ToggleGroup>
      </FlexItem>
      <FlexItem>
        {entries.length === 0 ? (
          <Content component="small">
            {all.length === 0 ? "Nothing recorded yet." : "No activity of this type yet."}
          </Content>
        ) : (
          <DataList aria-label="Recent activity" isCompact gridBreakpoint="none">
            {shown.map((entry, i) => {
              const category = activityCategory(entry);
              // Operator changes show the build; other details are free text.
              const detail = entry.build || (category === "operator" ? "" : entry.detail);
              return (
                <DataListItem key={`${entry.timestamp}-${i}`} aria-labelledby={`activity-${i}`}>
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="main">
                          <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                            <FlexItem>
                              <Icon isInline status={entry.success ? "success" : "danger"}>
                                {entry.success ? <CheckCircleIcon aria-label="Succeeded" /> : <TimesCircleIcon aria-label="Failed" />}
                              </Icon>
                            </FlexItem>
                            <FlexItem id={`activity-${i}`}>
                              <strong>{activityLabel(entry)}</strong>
                              {!entry.success && <span className="rhoai-subtle"> (failed)</span>}
                            </FlexItem>
                          </Flex>
                          {detail && (
                            <div className={entry.build ? "rhoai-build-id" : undefined} style={{ marginInlineStart: "1.5rem", overflowWrap: "anywhere" }}>
                              <Content component="small">{detail}</Content>
                            </div>
                          )}
                        </DataListCell>,
                        <DataListCell key="who" isFilled={false} alignRight>
                          <Content component="small" style={{ whiteSpace: "nowrap" }}>
                            {entry.user}
                            <br />
                            <time dateTime={entry.timestamp} title={new Date(entry.timestamp).toLocaleString()} className="rhoai-subtle">
                              {formatRelativeTime(entry.timestamp)}
                            </time>
                          </Content>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                </DataListItem>
              );
            })}
          </DataList>
        )}
      </FlexItem>
      {entries.length > PAGE_SIZE && (
        <FlexItem>
          <Button variant="link" isInline onClick={() => setExpanded(!expanded)}>
            {expanded ? "Show fewer" : `Show all ${entries.length}`}
          </Button>
        </FlexItem>
      )}
    </Flex>
  );
});
ActivityLog.displayName = "ActivityLog";
