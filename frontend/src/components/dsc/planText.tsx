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

/** The blocked removals and warnings of a plan. */
export const PlanAlerts: React.FC<{ plan: DSCPlan }> = ({ plan }) => (
  <Stack hasGutter>
    {(plan.blocked?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="danger" isInline title="This change is not possible now">
          <Content component="p">These components must not be removed yet; nothing is changed while any is listed. Resolve the reasons and try again.</Content>
          <List>{plan.blocked!.map((b) => <ListItem key={b.component}><code>{b.component}</code>: {b.reasons.join(" ")}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
    {(plan.warnings?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="warning" isInline title="Before you confirm">
          <List>{plan.warnings!.map((w) => <ListItem key={w}>{w}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
  </Stack>
);
