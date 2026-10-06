import React from "react";
import { MenuToggle } from "@patternfly/react-core";
import { ActionsColumn, type IAction } from "@patternfly/react-table";
import EllipsisVIcon from "@patternfly/react-icons/dist/esm/icons/ellipsis-v-icon";

/**
 * A table row's actions in a kebab menu (PF ActionsColumn), with a toggle
 * named after the row ("Actions for rhoai-3.6") instead of "Kebab toggle".
 * Rows keep one height whatever actions they have.
 */
export const RowActions: React.FC<{ items: IAction[]; rowName: string }> = ({ items, rowName }) => (
  <ActionsColumn
    items={items}
    popperProps={{ position: "right" }}
    actionsToggle={({ onToggle, isOpen, isDisabled, toggleRef }) => (
      <MenuToggle
        ref={toggleRef}
        variant="plain"
        onClick={onToggle}
        isExpanded={isOpen}
        isDisabled={isDisabled}
        aria-label={`Actions for ${rowName}`}
        icon={<EllipsisVIcon />}
      />
    )}
  />
);
