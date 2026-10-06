import React, { useState } from "react";
import {
  Badge,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  DataList,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  EmptyState,
  EmptyStateBody,
  Flex,
  FlexItem,
  Icon,
  MenuToggle,
  Select,
  SelectList,
  SelectOption,
  Title,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import TimesCircleIcon from "@patternfly/react-icons/dist/esm/icons/times-circle-icon";
import type { ActivityEntry } from "../types";
import { RelativeTime } from "./RelativeTime";
import { TruncatedText } from "./LongText";

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
  if (/minio|mlflow|pipeline/.test(entry.action)) return "test-resources";
  if (/dashboard|deploy-pr/.test(entry.action)) return "dashboard-dev";
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
  { id: "test-resources", label: "Test resources" },
  { id: "setup", label: "Setup" },
  { id: "diagnostics", label: "Diagnostics" },
  { id: "all", label: "All" },
];

const PAGE_SIZE = 5;

interface ActivityLogProps {
  activity?: ActivityEntry[];
  /** Category shown first (default: operator changes). */
  defaultCategory?: string;
}

/**
 * Who changed what on the cluster, newest first, as the "Recent activity"
 * card. The backend labels each entry, groups it by category (it keeps 50
 * operator entries apart from the noisier Dashboard Dev ones) and names the
 * build of operator changes. The category filter is a select in the card
 * header (PF: select menus for more than a few options).
 */
export const ActivityLog: React.FC<ActivityLogProps> = React.memo(({ activity, defaultCategory = "operator" }) => {
  const [filter, setFilter] = useState(defaultCategory);
  const [filterOpen, setFilterOpen] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const all = [...(activity ?? [])].reverse();
  const counts = new Map<string, number>();
  for (const e of all) counts.set(activityCategory(e), (counts.get(activityCategory(e)) ?? 0) + 1);
  const countOf = (id: string) => (id === "all" ? all.length : counts.get(id) ?? 0);
  const entries = filter === "all" ? all : all.filter((e) => activityCategory(e) === filter);
  const shown = expanded ? entries : entries.slice(0, PAGE_SIZE);
  const options = FILTERS.filter((f) => f.id === "all" || f.id === filter || countOf(f.id) > 0);
  const current = FILTERS.find((f) => f.id === filter) ?? FILTERS[0];

  const filterSelect = (
    <Select
      isOpen={filterOpen}
      selected={filter}
      onOpenChange={setFilterOpen}
      onSelect={(_e, value) => {
        setFilter(String(value));
        setExpanded(false);
        setFilterOpen(false);
      }}
      popperProps={{ position: "right" }}
      toggle={(ref) => (
        <MenuToggle
          ref={ref}
          onClick={() => setFilterOpen(!filterOpen)}
          isExpanded={filterOpen}
          aria-label={`Activity type: ${current.label}`}
          badge={<Badge isRead>{countOf(current.id)}</Badge>}
        >
          {current.label}
        </MenuToggle>
      )}
      shouldFocusToggleOnSelect
    >
      <SelectList aria-label="Activity types">
        {options.map((f) => (
          <SelectOption key={f.id} value={f.id}>
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
              <FlexItem>{f.label}</FlexItem>
              <FlexItem><Badge isRead>{countOf(f.id)}</Badge></FlexItem>
            </Flex>
          </SelectOption>
        ))}
      </SelectList>
    </Select>
  );

  return (
    <Card isFullHeight>
      <CardHeader actions={all.length > 0 ? { actions: filterSelect, hasNoOffset: true } : undefined}>
        <CardTitle><Title headingLevel="h2" size="lg">Recent activity</Title></CardTitle>
      </CardHeader>
      <CardBody>
        {entries.length === 0 ? (
          <EmptyState headingLevel="h3" titleText={all.length === 0 ? "No activity yet" : "No activity of this type yet"} variant="xs">
            <EmptyStateBody>
              {all.length === 0 ? "Updates, setups and Dashboard Dev changes made with this tool appear here." : "Choose another type above."}
            </EmptyStateBody>
          </EmptyState>
        ) : (
          <>
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
                          <DataListCell key="icon" isIcon>
                            <Icon status={entry.success ? "success" : "danger"}>
                              {entry.success ? <CheckCircleIcon aria-label="Succeeded" /> : <TimesCircleIcon aria-label="Failed" />}
                            </Icon>
                          </DataListCell>,
                          <DataListCell key="main">
                            <div id={`activity-${i}`}>
                              <strong>{activityLabel(entry)}</strong>
                              {!entry.success && !/failed$/i.test(activityLabel(entry)) && <span className="pf-v6-u-text-color-subtle"> (failed)</span>}
                            </div>
                            {detail && entry.build && (
                              <div className="pf-v6-u-font-family-monospace pf-v6-u-font-size-sm">
                                {entry.build.split(" · ").map((part, j) => (
                                  <React.Fragment key={j}>
                                    {j > 0 && " · "}
                                    <span className="pf-v6-u-text-nowrap">{part}</span>
                                  </React.Fragment>
                                ))}
                              </div>
                            )}
                            {detail && !entry.build && <div className="pf-v6-u-font-size-sm pf-v6-u-text-break-word">{detail}</div>}
                            {!entry.success && entry.reason && (
                              <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">
                                <TruncatedText>{entry.reason}</TruncatedText>
                              </div>
                            )}
                          </DataListCell>,
                          <DataListCell key="who" isFilled={false} alignRight>
                            <div className="pf-v6-u-font-size-sm pf-v6-u-text-nowrap">{entry.user}</div>
                            <RelativeTime date={entry.timestamp} />
                          </DataListCell>,
                        ]}
                      />
                    </DataListItemRow>
                  </DataListItem>
                );
              })}
            </DataList>
            {entries.length > PAGE_SIZE && (
              <Button variant="link" className="pf-v6-u-mt-sm" onClick={() => setExpanded(!expanded)}>
                {expanded ? "Show fewer" : `Show all ${entries.length}`}
              </Button>
            )}
          </>
        )}
      </CardBody>
    </Card>
  );
});
ActivityLog.displayName = "ActivityLog";
