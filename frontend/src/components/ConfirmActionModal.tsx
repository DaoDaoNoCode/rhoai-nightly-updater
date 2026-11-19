import React from "react";
import {
  Alert,
  Button,
  Content,
  ExpandableSection,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from "@patternfly/react-core";

export interface ConfirmActionModalProps {
  isOpen: boolean;
  title: string;
  /** Exactly what changes on the cluster, one item per object or effect. */
  changes: React.ReactNode[];
  /**
   * The outcome in words, shown first. With it, the object list moves into
   * a collapsed "Details" section: the facts stay one click away.
   */
  summary?: React.ReactNode;
  /** Optional extra text (who is affected, how to undo). */
  children?: React.ReactNode;
  /** Data that is lost, shown as a danger alert. */
  dataLoss?: React.ReactNode;
  /** A choice that changes what is deleted (a checkbox), shown before the data-loss alert. */
  choice?: React.ReactNode;
  confirmLabel: string;
  confirmVariant?: "primary" | "danger";
  isLoading?: boolean;
  /** Disable Confirm (for example while the preconditions load). */
  confirmDisabled?: boolean;
  /** With a summary, show the changes after it, open, instead of under "Details". */
  changesOpen?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * Confirmation for an action that changes a shared cluster (A04-4, A08-4).
 * It lists what will change, so the user decides with the facts in front
 * of them; Cancel is always available and Escape closes the dialog.
 */
export const ConfirmActionModal: React.FC<ConfirmActionModalProps> = ({
  isOpen, title, changes, summary, children, dataLoss, choice, confirmLabel, confirmVariant = "primary", isLoading, confirmDisabled, changesOpen, onConfirm, onCancel,
}) => {
  const titleId = React.useId();
  const [detailsOpen, setDetailsOpen] = React.useState(false);
  const changeList = (
    <>
      <Content component="p">This changes the shared cluster:</Content>
      <List>
        {changes.map((change, i) => <ListItem key={i}>{change}</ListItem>)}
      </List>
    </>
  );
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen={isOpen} onClose={() => !isLoading && onCancel()}>
      <ModalHeader title={title} labelId={titleId} titleIconVariant={confirmVariant === "danger" ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>{summary ?? changeList}</StackItem>
          {choice && <StackItem>{choice}</StackItem>}
          {dataLoss && (
            <StackItem>
              <Alert component="p" variant="danger" isInline title="Data is deleted and cannot be recovered">{dataLoss}</Alert>
            </StackItem>
          )}
          {children && <StackItem>{children}</StackItem>}
          {summary && changesOpen && changes.length > 0 && <StackItem>{changeList}</StackItem>}
          {summary && !changesOpen && changes.length > 0 && (
            <StackItem>
              <ExpandableSection toggleText="Details" isExpanded={detailsOpen} onToggle={(_e, v) => setDetailsOpen(v)}>{changeList}</ExpandableSection>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant={confirmVariant} onClick={onConfirm} isLoading={isLoading} isDisabled={isLoading || confirmDisabled}>{confirmLabel}</Button>
        <Button variant="link" onClick={onCancel} isDisabled={isLoading}>Cancel</Button>
      </ModalFooter>
    </Modal>
  );
};
