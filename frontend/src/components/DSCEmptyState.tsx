import React, { useState } from "react";
import { Link } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CodeBlock,
  CodeBlockCode,
  Content,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Skeleton,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import CubesIcon from "@patternfly/react-icons/dist/esm/icons/cubes-icon";
import type { OperationResponse } from "../types";
import { createDSC, getDSCPreview, trackFeature, type DSCPreviewResponse } from "../services/api";
import { errorResult, outcomeTitle, outcomeVariant } from "../outcomes";
import { TooltipButton, NO_PERMISSION_REASON } from "./TooltipButton";
import { CHECKING_PERMISSIONS_REASON } from "../hooks/usePermissions";

interface DSCEmptyStateProps {
  /** "no-dsc" or "no-crd" (from /api/components dscState). */
  dscState: string;
  operatorVersion?: string;
  operatorPhase?: string;
  canMutate: boolean;
  permissionsLoaded: boolean;
  onCreated: () => void;
}

/**
 * What to do when there is no DataScienceCluster (A05-8). "no-dsc": the
 * operator is installed, so offer to create the DSC from the operator's own
 * example after showing it. "no-crd": the DSC API does not exist, so RHOAI
 * is not installed (or is still installing).
 */
export const DSCEmptyState: React.FC<DSCEmptyStateProps> = ({ dscState, operatorVersion, operatorPhase, canMutate, permissionsLoaded, onCreated }) => {
  const [open, setOpen] = useState(false);
  const [preview, setPreview] = useState<(DSCPreviewResponse & { yaml: string }) | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [creating, setCreating] = useState(false);
  const [result, setResult] = useState<OperationResponse | null>(null);

  const openPreview = async () => {
    setOpen(true);
    setPreview(null);
    setPreviewError("");
    try {
      setPreview(await getDSCPreview());
    } catch (e) {
      setPreviewError(errorResult(e, "Could not load the DataScienceCluster defaults").message);
    }
  };

  const create = async () => {
    trackFeature("create_dsc");
    setCreating(true);
    let res: OperationResponse;
    try {
      res = await createDSC();
    } catch (e) {
      res = errorResult(e, "Could not create the DataScienceCluster");
    }
    setCreating(false);
    setOpen(false);
    setResult(res);
    if (res.success) onCreated();
  };

  if (dscState === "no-crd") {
    const installing = !!operatorPhase && operatorPhase !== "Succeeded";
    return (
      <PageSection>
        <Card>
          <CardBody>
            <EmptyState headingLevel="h2" icon={CubesIcon} titleText={installing ? "RHOAI is still installing" : "RHOAI is not installed"}>
              <EmptyStateBody>
                {installing
                  ? `The rhods-operator ${operatorVersion ?? ""} install is in phase ${operatorPhase}. The DataScienceCluster API appears when the install finishes; this page refreshes on its own.`
                  : "This cluster has no DataScienceCluster API (datasciencecluster.opendatahub.io), so the RHOAI operator is not installed. Install a nightly build from the Dashboard first."}
              </EmptyStateBody>
              {!installing && (
                <EmptyStateFooter>
                  <EmptyStateActions>
                    <Link to="/">Go to the Dashboard to install RHOAI</Link>
                  </EmptyStateActions>
                </EmptyStateFooter>
              )}
            </EmptyState>
          </CardBody>
        </Card>
      </PageSection>
    );
  }

  const disabledReason = !permissionsLoaded ? CHECKING_PERMISSIONS_REASON : !canMutate ? NO_PERMISSION_REASON : null;
  return (
    <PageSection>
      <Card>
        <CardBody>
          <EmptyState headingLevel="h2" icon={CubesIcon} titleText="No DataScienceCluster yet">
            <EmptyStateBody>
              The RHOAI operator{operatorVersion ? ` ${operatorVersion}` : ""} is installed{operatorPhase ? ` (${operatorPhase})` : ""}, but no
              DataScienceCluster exists, so it deploys no components. Create one from the operator&apos;s own defaults; you can change
              component states afterwards.
            </EmptyStateBody>
            <EmptyStateFooter>
              <EmptyStateActions>
                <TooltipButton variant="primary" onClick={openPreview} disabledReason={disabledReason} isLoading={creating}>
                  Preview and create DataScienceCluster
                </TooltipButton>
              </EmptyStateActions>
              {result && (
                <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "Could not create the DataScienceCluster")}>
                  {result.success ? undefined : result.message}
                </Alert>
              )}
            </EmptyStateFooter>
          </EmptyState>
        </CardBody>
      </Card>

      <Modal aria-labelledby="create-dsc-title" variant={ModalVariant.large} isOpen={open} onClose={() => !creating && setOpen(false)}>
        <ModalHeader title="Create DataScienceCluster" labelId="create-dsc-title" description={preview?.sourceDescription ? `Source: ${preview.sourceDescription}` : undefined} />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                This creates the DataScienceCluster below on the shared cluster. The operator then deploys every component
                set to <code>Managed</code>. If a DataScienceCluster already exists, nothing is changed.
              </Content>
            </StackItem>
            <StackItem>
              {previewError ? (
                <Alert component="p" variant="danger" title="Could not load the defaults" isInline>{previewError}</Alert>
              ) : preview ? (
                <CodeBlock><CodeBlockCode>{preview.yaml}</CodeBlockCode></CodeBlock>
              ) : (
                <Skeleton height="12rem" screenreaderText="Loading the DataScienceCluster defaults" />
              )}
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={create} isLoading={creating} isDisabled={creating || !preview || !!previewError || !canMutate}>Create</Button>
          <Button variant="link" onClick={() => setOpen(false)} isDisabled={creating}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </PageSection>
  );
};
