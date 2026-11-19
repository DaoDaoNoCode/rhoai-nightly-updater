import React, { useState } from "react";
import {
  Alert,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { ServerOperation } from "../types";
import { useClusterStatus, useOperation } from "../state/AppState";
import { sentence } from "../build";
import { RelativeTime } from "./RelativeTime";

/**
 * The progress of an operation this tab has no dialog for: started in
 * another tab, by another user, or by this tab before it left the page that
 * runs it. The backend records only who started it, when, what it acts on
 * and its latest progress (GET /api/operation), not each step, so that is
 * what this shows, refreshed by the app's operation poll. Once the
 * operation ends it says how, when the backend reports it (lastCompleted).
 */
export const ServerOperationModal: React.FC<{ operation: ServerOperation; onClose: () => void }> = ({ operation, onClose }) => {
  const titleId = React.useId();
  const { server } = useOperation();
  const { status } = useClusterStatus();
  const live = server.inProgress && server.operation?.id === operation.id ? server.operation : null;
  // The latest answer about it, kept once it ends.
  const [seen, setSeen] = useState(operation);
  if (live && live !== seen) setSeen(live);
  const op = live ?? seen;
  const last = server.lastCompleted?.id === operation.id ? server.lastCompleted : null;
  const you = !!op.user && op.user === status?.cluster.user;
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.small} isOpen onClose={onClose}>
      <ModalHeader title={op.label || op.type} labelId={titleId} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            <DescriptionList isHorizontal isCompact aria-label="What the server records">
              <DescriptionListGroup>
                <DescriptionListTerm>State</DescriptionListTerm>
                <DescriptionListDescription>
                  {live ? (
                    <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
                      <FlexItem><Spinner size="sm" aria-hidden="true" /></FlexItem>
                      <FlexItem>Running</FlexItem>
                    </Flex>
                  ) : "Finished"}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Started by</DescriptionListTerm>
                <DescriptionListDescription>{op.user ? `${op.user}${you ? " (you)" : ""}` : "Unknown"}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Started</DescriptionListTerm>
                <DescriptionListDescription><RelativeTime date={op.startedAt} size="inherit" /></DescriptionListDescription>
              </DescriptionListGroup>
              {op.message && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Latest progress</DescriptionListTerm>
                  <DescriptionListDescription>
                    {sentence(op.message)}
                    {op.updatedAt && <> <span className="pf-v6-u-text-color-subtle">(<RelativeTime date={op.updatedAt} size="inherit" />)</span></>}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {op.target && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Acts on</DescriptionListTerm>
                  <DescriptionListDescription><code className="pf-v6-u-text-break-word">{op.target}</code></DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {op.remote && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Runs on</DescriptionListTerm>
                  <DescriptionListDescription>Updater pod {op.pod || "(unknown)"}</DescriptionListDescription>
                </DescriptionListGroup>
              )}
            </DescriptionList>
          </StackItem>
          {live && (
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">
                This page did not start it, so it has no step list: the server keeps only the latest progress, shown here and updated every few seconds.
              </Content>
            </StackItem>
          )}
          {!live && (
            <StackItem>
              {last ? (
                <Alert component="p" isInline isLiveRegion variant={last.success ? "success" : "danger"} title={last.success ? "It finished" : "It stopped"}>
                  {last.message ? sentence(last.message) : undefined}
                </Alert>
              ) : (
                <Alert component="p" isInline isLiveRegion variant="info" title="It is no longer running">
                  The server did not say how it ended. The page it changes shows the result.
                </Alert>
              )}
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
