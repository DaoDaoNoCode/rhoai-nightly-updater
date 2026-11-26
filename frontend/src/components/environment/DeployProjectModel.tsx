import React, { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Content,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { CatalogModel, ModelExposures, ProjectModelRequest } from "../../types";
import { getModelExposures, toApiError, type ApiError } from "../../services/api";
import { useProjectCapability } from "./projectActions";
import { LinesSkeleton } from "./Loading";
import { runtimeImageWords } from "./modelsConfirm";

/** What the dialog starts: a project model setup. */
export interface DeployProjectModelChoice {
  title: string;
  req: ProjectModelRequest;
}

/** Who deploys what this dialog does not (pkg/cluster/internal/serving ClusterAdminModelWords). */
export const CLUSTER_ADMIN_MODELS = "A cluster-admin deploys models reached through the inference gateway or with token authentication, on the Environment page's Models tab.";

/**
 * Deploy a model of the catalog in one of the user's projects (the non-admin design, B6): reached
 * only inside the cluster, without token authentication, as the dashboard sets up a model without
 * "Require token authentication". Through the inference gateway, or with token authentication, a
 * model is a cluster-admin's (the Models tab). The answer of GET /api/resources/models/exposures gives
 * the in-cluster warning and whether the cluster has room for the model.
 */
export const DeployProjectModel: React.FC<{
  project: string;
  catalog: CatalogModel[];
  onDeploy: (choice: DeployProjectModelChoice) => void;
  onClose: () => void;
}> = ({ project, catalog, onDeploy, onClose }) => {
  const [model, setModel] = useState(() => (catalog.find((m) => m.default) ?? catalog[0])?.name ?? "");
  const [answer, setAnswer] = useState<{ model: string; exposures: ModelExposures | null; error: ApiError | null }>({ model: "", exposures: null, error: null });
  const [releasedImage, setReleasedImage] = useState(false);
  const setupCap = useProjectCapability(project, "models.setup");

  useEffect(() => {
    if (!model) return;
    let live = true;
    getModelExposures(project, model).then(
      (exposures) => { if (live) setAnswer({ model, exposures, error: null }); },
      (e: unknown) => { if (live) setAnswer({ model, exposures: null, error: toApiError(e) }); },
    );
    return () => { live = false; };
  }, [project, model]);

  const exposures = answer.model === model ? answer.exposures : null;
  const error = answer.model === model ? answer.error : null;
  const internal = exposures?.options.find((o) => o.key === "internal");
  const entry = catalog.find((m) => m.name === model);
  const canDeploy = !!internal && setupCap.allowed;
  // The image the plan already resolves to: the choice of the released image is offered only while it would run the preset's.
  const plan = exposures?.imagePlan;
  const offerReleased = !plan || plan === "preset";

  return (
    <Modal variant={ModalVariant.medium} isOpen onClose={onClose} aria-labelledby="user-env-deploy-model-title">
      <ModalHeader title={`Deploy a model in ${project}`} labelId="user-env-deploy-model-title" />
      <ModalBody>
        <Form onSubmit={(e) => e.preventDefault()}>
          <FormGroup label="Model" fieldId="user-env-deploy-model">
            <FormSelect id="user-env-deploy-model" value={model} onChange={(_e, v) => setModel(v)} aria-label="Model">
              {catalog.map((m) => <FormSelectOption key={m.name} value={m.name} label={`${m.displayName}: ${m.cpu} CPUs, ${m.memoryRequest} of memory`} />)}
            </FormSelect>
          </FormGroup>
          <FormGroup label="How it is reached" fieldId="user-env-deploy-exposure">
            {error && <Alert variant="warning" isInline component="div" title="Cannot check the model in this project"><Content component="p">{error.message}</Content></Alert>}
            {!error && !exposures && <LinesSkeleton what="how the model is reached" lines={2} />}
            {internal && (
              <Stack hasGutter>
                <StackItem>
                  <Content component="p"><strong>{internal.title}</strong>, without token authentication. {internal.description}</Content>
                </StackItem>
                {internal.warning && <StackItem><HelperText><HelperTextItem variant="warning">{internal.warning}</HelperTextItem></HelperText></StackItem>}
                <StackItem><HelperText><HelperTextItem>{CLUSTER_ADMIN_MODELS}</HelperTextItem></HelperText></StackItem>
              </Stack>
            )}
          </FormGroup>
          <FormGroup fieldId="user-env-deploy-image" role="group" aria-label="Runtime image">
            {plan && <Content component="p" className="pf-v6-u-mb-sm">{runtimeImageWords(plan, exposures?.imagePlanReason)}</Content>}
            {offerReleased && (
              <Checkbox id="user-env-deploy-released" label="Use the newest released Red Hat image"
                description="Instead of the cluster preset's image; for a nightly build whose runtime image is not published yet."
                isChecked={releasedImage} onChange={(_e, checked) => setReleasedImage(checked)} />
            )}
          </FormGroup>
        </Form>
        <Stack hasGutter className="pf-v6-u-mt-md">
          {exposures?.capacity === "may-not-fit" && (
            <StackItem>
              <Alert variant="warning" isInline component="div" title="The cluster may not have room for it">
                <Content component="p">No worker seems to have the model&apos;s CPU and memory free. It may stay pending until room frees up; the setup still runs.</Content>
              </Alert>
            </StackItem>
          )}
          {!setupCap.allowed && setupCap.reason && <StackItem><HelperText><HelperTextItem variant="warning">{setupCap.reason}</HelperTextItem></HelperText></StackItem>}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" isDisabled={!canDeploy} onClick={() => onDeploy({
          title: `Deploy ${entry?.displayName ?? model} in ${project}`,
          req: { action: "setup", project, model, exposure: "internal", ...(releasedImage && offerReleased ? { releasedImage: true } : {}) },
        })}>Deploy</Button>
        <Button variant="link" onClick={onClose}>Cancel</Button>
      </ModalFooter>
    </Modal>
  );
};
