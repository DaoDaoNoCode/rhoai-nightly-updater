import React from "react";
import { Alert, Content, List, ListItem, Stack, StackItem } from "@patternfly/react-core";
import type { DSCChange, DSCPlan } from "../../types";

/** A change of a plan in words. */
export function changeText(ch: DSCChange): React.ReactNode {
  const from = ch.fromSet ? ch.from : "not set";
  const to = ch.toSet ? ch.to : "not set";
  const effect = ch.effect === "deploys" ? " (deploys it)" : ch.effect === "removes" ? " (removes it)" : "";
  if (from === to) {
    return <><code>{ch.path}</code> stays {from}{ch.effect === "removes" ? ", but deploys nothing: its parent is no longer Managed" : ch.effect === "deploys" ? " and is deployed again: its parent is Managed" : ""}</>;
  }
  return <><code>{ch.path}</code>: {from} → {to}{effect}</>;
}

/** Paths as a comma-separated list of code. */
export const PathList: React.FC<{ paths: string[] }> = ({ paths }) => (
  <>{paths.map((p, i) => <React.Fragment key={p}>{i > 0 && ", "}<code>{p}</code></React.Fragment>)}</>
);

/** A duration the backend sends in seconds, in words: "10 minutes", "1 minute 30 seconds", "45 seconds". */
export function durationText(seconds: number): string {
  const unit = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
  const min = Math.floor(seconds / 60);
  const sec = seconds % 60;
  if (min === 0) return unit(sec, "second");
  return sec === 0 ? unit(min, "minute") : `${unit(min, "minute")} ${unit(sec, "second")}`;
}

/** The removals a plan must not make now, one line per component; nothing when there are none. */
export const BlockedAlert: React.FC<{ plan: DSCPlan }> = ({ plan }) => {
  const blocked = plan.blocked ?? [];
  if (blocked.length === 0) return null;
  return (
    <Alert component="div" variant="danger" isInline title="This change is not possible now">
      <Content component="p">Nothing is changed while any of these is listed:</Content>
      <List aria-label="Why the change is not possible">
        {blocked.map((b) => <ListItem key={b.component}><code>{b.component}</code>: {b.reasons.join(" ")}</ListItem>)}
      </List>
    </Alert>
  );
};

/** A plan's warnings; nothing when there are none. */
export const WarningsAlert: React.FC<{ plan: DSCPlan }> = ({ plan }) => {
  const warnings = plan.warnings ?? [];
  if (warnings.length === 0) return null;
  return (
    <Alert component="div" variant="warning" isInline title="Before you confirm">
      <List>{warnings.map((w) => <ListItem key={w}>{w}</ListItem>)}</List>
    </Alert>
  );
};

/** The blocked removals and warnings of a plan. */
export const PlanAlerts: React.FC<{ plan: DSCPlan }> = ({ plan }) => (
  <Stack hasGutter>
    {(plan.blocked?.length ?? 0) > 0 && <StackItem><BlockedAlert plan={plan} /></StackItem>}
    {(plan.warnings?.length ?? 0) > 0 && <StackItem><WarningsAlert plan={plan} /></StackItem>}
  </Stack>
);
