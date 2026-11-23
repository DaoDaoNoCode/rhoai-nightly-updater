import React from "react";
import { Alert, Button, Content, List, ListItem } from "@patternfly/react-core";

/** A single resource this tool manages while its project no longer counts as a data science project. */
export interface OutsideItem {
  key: string;
  text: React.ReactNode;
  onDelete: () => void;
}

/**
 * Surfaces tool-created resources living in projects the data science picker
 * omits—never eligible or no longer labeled as such. They still exist on the
 * cluster; each row names one and, when not read-only, exposes Delete.
 */
export const OutsideProjectsAlert: React.FC<{ items: OutsideItem[]; readOnly: boolean; reason: string | null }> = ({ items, readOnly, reason }) => {
  if (items.length === 0) return null;
  return (
    <Alert variant="warning" isInline component="div" title="Set up by this tool outside data science projects">
      <Content component="p">
        The project picker lists data science projects only. These are in a project that is no longer a data science project (its RHOAI dashboard label was removed), and they are still on the cluster:
      </Content>
      <List>
        {items.map((i) => (
          <ListItem key={i.key}>
            {i.text}
            {!readOnly && <>{" "}<Button variant="link" isInline isDanger isAriaDisabled={!!reason} onClick={i.onDelete}>Delete...</Button></>}
          </ListItem>
        ))}
      </List>
    </Alert>
  );
};
