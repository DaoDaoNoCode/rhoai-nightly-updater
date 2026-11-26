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
  Radio,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import type { CatalogModel, ModelExposures, ProjectModelRequest } from "../../types";
import { getModelExposures, toApiError, type ApiError } from "../../services/api";
import { useProjectCapability } from "./projectActions";
import { LinesSkeleton } from "./Loading";

/** What the dialog starts: a project model setup. */
export interface DeployProjectModelChoice {
  title: string;
  req: ProjectModelRequest;
}

/**
 * Deploy a model of the catalog in one of the user's projects (the non-admin design, B6): inside the
 * cluster, or through the cluster's inference gateway when it already admits the project, which
 * needs token authentication. Token authentication creates a Role and a RoleBinding, so it needs
 * the project's admin role: the capabilities say whether the user has it, and its checkbox is
 * disabled with the reason otherwise. MaaS and adding the project to the gateway stay a
 * cluster-admin's (the Models tab).
 */
export const DeployProjectModel: React.FC<{
  project: string;
  catalog: CatalogModel[];
  onDeploy: (choice: DeployProjectModelChoice) => void;
  onClose: () => void;
}> = ({ project, catalog, onDeploy, onClose }) => {
  const [model, setModel] = useState(() => (catalog.find((m) => m.default) ?? catalog[0])?.name ?? "");
  const [answer, setAnswer] = useState<{ model: string; exposures: ModelExposures | null; error: ApiError | null }>({ model: "", exposures: null, error: null });
  const [exposure, setExposure] = useState<"internal" | "default-gateway">("internal");
  const [tokenAuth, setTokenAuth] = useState(false);
  const [releasedImage, setReleasedImage] = useState(false);
  const tokenCap = useProjectCapability(project, "models.setup-token");
  const setupCap = useProjectCapability(project, "models.setup");

  useEffect(() => {
    if (!model) return;
    let live = true;
    getModelExposures(project, model).then(
      (exposures) => {
        if (!live) return;
        setAnswer({ model, exposures, error: null });
      },
      (e: unknown) => { if (live) setAnswer({ model, exposures: null, error: toApiError(e) }); },
    );
    return () => { live = false; };
  }, [project, model]);

  const exposures = answer.model === model ? answer.exposures : null;
  const error = answer.model === model ? answer.error : null;
  const gateway = exposures?.options.find((o) => o.key === "default-gateway");
  const internal = exposures?.options.find((o) => o.key === "internal");
  const gatewayUsable = !!gateway?.available && tokenCap.allowed;
  // The user's choices stay as they made them: when a later answer (another
  // model, a refreshed capability) no longer allows one, the dialog says so
  // and Deploy waits for them to choose again; it never switches silently.
  const withToken = exposure === "default-gateway" || tokenAuth;
  const gatewayGone = exposure === "default-gateway" && !!exposures && !gatewayUsable;
  const tokenGone = withToken && !tokenCap.allowed && !!exposures;
  const entry = catalog.find((m) => m.name === model);
  const canDeploy = !!exposures && !gatewayGone && !tokenGone && (withToken ? tokenCap.allowed : setupCap.allowed);

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
          <FormGroup label="How it is reached" fieldId="user-env-deploy-exposure" role="radiogroup" isStack>
            {error && <Alert variant="warning" isInline component="div" title="Cannot check how the model can be reached"><Content component="p">{error.message}</Content></Alert>}
            {!error && !exposures && <LinesSkeleton what="how the model can be reached" lines={2} />}
            {exposures && gateway && (
              <Radio id="user-env-deploy-exposure-gateway" name="user-env-deploy-exposure" isChecked={exposure === "default-gateway"} isDisabled={!gatewayUsable}
                onChange={() => setExposure("default-gateway")} label={gateway.title} description={gateway.description}
                body={!gateway.available ? (
                  <HelperText><HelperTextItem variant="warning">{gateway.reason ?? "Not available now."}</HelperTextItem></HelperText>
                ) : !tokenCap.allowed ? (
                  <HelperText><HelperTextItem variant="warning">It needs token authentication, which your role in {project} does not allow (below).</HelperTextItem></HelperText>
                ) : gateway.address ? (
                  <HelperText><HelperTextItem>Its address: <code>{gateway.address}</code></HelperTextItem></HelperText>
                ) : undefined} />
            )}
            {exposures && internal && (
              <Radio id="user-env-deploy-exposure-internal" name="user-env-deploy-exposure" isChecked={exposure === "internal"}
                onChange={() => setExposure("internal")} label={internal.title} description={internal.description}
                body={exposure === "internal" && internal.warning ? <HelperText><HelperTextItem variant="warning">{internal.warning}</HelperTextItem></HelperText> : undefined} />
            )}
          </FormGroup>
          <FormGroup fieldId="user-env-deploy-token" isStack>
            <Checkbox id="user-env-deploy-token" label="Require token authentication"
              description={exposure === "default-gateway"
                ? "Needed through the gateway: each call needs a token. Creates a ServiceAccount, a Role, a RoleBinding and a token Secret in the project, as the dashboard does."
                : "Creates a ServiceAccount, a Role, a RoleBinding and a token Secret in the project, as the dashboard does. Without it the model's token authentication is off."}
              isChecked={withToken} isDisabled={exposure === "default-gateway" || (!tokenCap.allowed && !tokenAuth)} onChange={(_e, checked) => setTokenAuth(checked)}
              body={!tokenCap.allowed ? <HelperText><HelperTextItem variant="warning">{tokenCap.reason}</HelperTextItem></HelperText> : undefined} />
            <Checkbox id="user-env-deploy-released" label="Use the newest released Red Hat image"
              description="Instead of the cluster preset's image; for a nightly build whose runtime image is not published yet."
              isChecked={releasedImage} onChange={(_e, checked) => setReleasedImage(checked)} />
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
          {gatewayGone && (
            <StackItem>
              <Alert variant="warning" isInline component="div" title="Choose how it is reached again">
                <Content component="p">The cluster&apos;s inference gateway can no longer be used for this model: {gateway?.available ? tokenCap.reason : (gateway?.reason ?? "it is not available now.")} Choose &quot;Only inside the cluster&quot;, or another model.</Content>
              </Alert>
            </StackItem>
          )}
          {!gatewayGone && tokenGone && (
            <StackItem>
              <Alert variant="warning" isInline component="div" title="Token authentication is no longer allowed">
                <Content component="p">{tokenCap.reason} Clear &quot;Require token authentication&quot; to deploy without it.</Content>
              </Alert>
            </StackItem>
          )}
          {!setupCap.allowed && setupCap.reason && <StackItem><HelperText><HelperTextItem variant="warning">{setupCap.reason}</HelperTextItem></HelperText></StackItem>}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" isDisabled={!canDeploy} onClick={() => onDeploy({
          title: `Deploy ${entry?.displayName ?? model} in ${project}`,
          req: { action: "setup", project, model, exposure, ...(withToken ? { tokenAuth: true } : {}), ...(releasedImage ? { releasedImage: true } : {}) },
        })}>Deploy</Button>
        <Button variant="link" onClick={onClose}>Cancel</Button>
      </ModalFooter>
    </Modal>
  );
};
