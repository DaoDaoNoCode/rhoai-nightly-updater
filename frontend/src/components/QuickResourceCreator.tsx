import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  DataList,
  DataListCell,
  DataListItem,
  DataListItemCells,
  DataListItemRow,
  Flex,
  FlexItem,
  Label,
  MenuToggle,
  MenuToggleElement,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Select,
  SelectList,
  SelectOption,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
  Tooltip,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import TimesIcon from "@patternfly/react-icons/dist/esm/icons/times-icon";
import type { OperationResponse, ResourcesStatus } from "../types";
import {
  getResourcesStatus,
  getDSProjects,
  setupMinIO,
  teardownMinIO,
  setupPipelineServer,
  teardownPipelineServer,
  trackFeature,
  setupMLflow,
  teardownMLflow,
  deployMLflowPR,
  revertMLflow,
} from "../services/api";

interface QuickResourceCreatorProps {
  canMutate: boolean;
}

export const QuickResourceCreator: React.FC<QuickResourceCreatorProps> = ({ canMutate }) => {
  const [resStatus, setResStatus] = useState<ResourcesStatus | null>(null);
  const [resAction, setResAction] = useState<string | null>(null);
  const [resResult, setResResult] = useState<OperationResponse | null>(null);
  const [teardownConfirm, setTeardownConfirm] = useState<string | null>(null);
  const [projectList, setProjectList] = useState<string[]>([]);
  const [addProjectOpen, setAddProjectOpen] = useState(false);
  const [setupConfirmProject, setSetupConfirmProject] = useState<string | null>(null);
  const [mlflowPR, setMlflowPR] = useState("");
  const [setupConfirm, setSetupConfirm] = useState<string | null>(null);

  const fetchResources = useCallback(async () => {
    try {
      const s = await getResourcesStatus();
      setResStatus(s);
    } catch {
      // non-critical
    }
  }, []);

  useEffect(() => { fetchResources(); }, [fetchResources]);

  const refreshProjects = useCallback(() => {
    getDSProjects().then((r) => setProjectList(r.projects || [])).catch(() => {});
  }, []);

  useEffect(() => { refreshProjects(); }, [refreshProjects]);

  const resourcesSettling = resStatus != null && (
    (resStatus.minio.deployed && !resStatus.minio.ready) ||
    resStatus.minio.message === "Terminating" ||
    (resStatus.mlflow.deployed && !resStatus.mlflow.ready) ||
    (resStatus.pipelineServers || []).some((ps) => ps.deployed && !ps.ready)
  );

  useEffect(() => {
    if (!resAction && !resourcesSettling) return;
    const id = setInterval(fetchResources, 5000);
    return () => clearInterval(id);
  }, [resAction, resourcesSettling, fetchResources]);

  const handleResourceAction = async (action: string, fn: () => Promise<OperationResponse>) => {
    trackFeature(action);
    setResAction(action);
    setResResult(null);
    try {
      const res = await fn();
      setResResult(res);
      setTeardownConfirm(null);
      fetchResources();
      refreshProjects();
    } catch (e) {
      setResResult({ success: false, message: e instanceof Error ? e.message : "Operation failed", logs: [] });
      setTeardownConfirm(null);
    } finally {
      setResAction(null);
    }
  };

  const availableProjects = projectList.filter(
    (p) => !(resStatus?.pipelineServers || []).some((ps) => ps.namespace === p)
  );

  const getTeardownLabel = (id: string | null): string => {
    switch (id) {
      case "minio": return "MinIO";
      case "mlflow": return "MLflow";
      default: return `Pipeline Server in ${id}`;
    }
  };

  const getTeardownWarning = (id: string | null): string => {
    switch (id) {
      case "minio":
        return "This deletes the entire 'minio' namespace including all stored data (pipeline artifacts, models, test files). This cannot be undone. Other projects using this MinIO as their S3 backend will lose access to their storage.";
      case "mlflow":
        return "This removes the MLflow CR and its storage (experiments, models, artifacts). The MLflow operator will clean up all resources.";
      default:
        return `This removes the pipeline server (DSPA), its database, and credentials from '${id}'. Existing pipeline runs will be lost. The namespace is preserved.`;
    }
  };

  const executeTeardown = (id: string) => {
    switch (id) {
      case "minio":
        handleResourceAction("teardown-minio", teardownMinIO);
        break;
      case "mlflow":
        handleResourceAction("teardown-mlflow", teardownMLflow);
        break;
      default:
        handleResourceAction("teardown-pipeline-server", () => teardownPipelineServer(id));
    }
  };

  const getSetupLabel = (id: string | null): string => {
    switch (id) {
      case "setup-minio": return "Set up MinIO";
      case "setup-mlflow": return "Set up MLflow";
      case "deploy-mlflow-pr": return "Deploy MLflow PR";
      case "revert-mlflow": return "Revert MLflow";
      default: return "";
    }
  };

  const getSetupWarning = (id: string | null): string => {
    switch (id) {
      case "setup-minio":
        return "This will create a MinIO deployment in the minio namespace with a pipeline bucket.";
      case "setup-mlflow":
        return "This will create an MLflow server in redhat-ods-applications.";
      case "deploy-mlflow-pr":
        return "This will replace the running MLflow server with PR image. The server will restart.";
      case "revert-mlflow":
        return "This will revert MLflow to the operator-managed image.";
      default:
        return "";
    }
  };

  const executeSetup = (id: string) => {
    setSetupConfirm(null);
    switch (id) {
      case "setup-minio":
        handleResourceAction("setup-minio", setupMinIO);
        break;
      case "setup-mlflow":
        handleResourceAction("setup-mlflow", setupMLflow);
        break;
      case "deploy-mlflow-pr":
        handleResourceAction("deploy-mlflow-pr", () => deployMLflowPR(parseInt(mlflowPR, 10)));
        break;
      case "revert-mlflow":
        handleResourceAction("revert-mlflow", revertMLflow);
        break;
    }
  };

  return (
    <>
      <Card>
        <CardTitle>
          <Title headingLevel="h3">Quick Resource Creator</Title>
        </CardTitle>
        <CardBody>
          <Content component="small" className="pf-v6-u-mb-md">
            Deploy test infrastructure with sensible defaults.
          </Content>

          {resResult && (
            <Alert
              variant={resResult.success ? "success" : "danger"}
              title={resResult.message}
              isInline
              className="pf-v6-u-mb-md"
              actionClose={<Button variant="plain" aria-label="Close" onClick={() => setResResult(null)}><TimesIcon /></Button>}
            />
          )}

          <Stack hasGutter>
            {/* Storage & Data */}
            <StackItem>
              <Content component="h4">Storage & Data</Content>
              <DataList isCompact aria-label="Storage resources">
                <DataListItem aria-labelledby="minio-item">
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="name" width={2} id="minio-item">
                          <strong>MinIO Object Storage</strong>
                          <Content component="small">Namespace: minio — S3 storage for pipeline artifacts, model data, and test files.</Content>
                        </DataListCell>,
                        <DataListCell key="status" width={1} alignRight>
                          <Flex justifyContent={{ default: "justifyContentFlexEnd" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                            {resStatus?.minio.message === "Terminating" ? (
                              <FlexItem><Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Terminating" />}>Terminating</Label></FlexItem>
                            ) : resStatus?.minio.ready ? (
                              <>
                                <FlexItem><Label isCompact color="green" icon={<CheckCircleIcon />}>Running</Label></FlexItem>
                                {resStatus.minio.uiRoute && (
                                  <FlexItem>
                                    <Button variant="link" isInline component="a" href={resStatus.minio.uiRoute} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">Console</Button>
                                  </FlexItem>
                                )}
                                <FlexItem>
                                  <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                    <Button variant="secondary" isDanger size="sm" onClick={() => setTeardownConfirm("minio")} isDisabled={!canMutate || !!resAction}>Tear down</Button>
                                  </Tooltip>
                                </FlexItem>
                              </>
                            ) : resStatus?.minio.deployed ? (
                              <>
                                <FlexItem><Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Starting" />}>{resStatus.minio.message}</Label></FlexItem>
                                <FlexItem>
                                  <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                    <Button variant="secondary" isDanger size="sm" onClick={() => setTeardownConfirm("minio")} isDisabled={!canMutate || !!resAction}>Tear down</Button>
                                  </Tooltip>
                                </FlexItem>
                              </>
                            ) : (
                              <FlexItem>
                                <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                  <Button variant="primary" size="sm" onClick={() => setSetupConfirm("setup-minio")} isDisabled={!canMutate || !!resAction || resStatus?.minio.message === "Terminating"} isLoading={resAction === "setup-minio"}>Set up</Button>
                                </Tooltip>
                              </FlexItem>
                            )}
                          </Flex>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                </DataListItem>
              </DataList>
            </StackItem>

            {/* MLflow */}
            <StackItem>
              <Content component="h4">MLflow</Content>
              <DataList isCompact aria-label="MLflow">
                <DataListItem aria-labelledby="mlflow-item">
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="name" width={2} id="mlflow-item">
                          <strong>MLflow Server</strong>
                          <Content component="small">Deploys MLflow in redhat-ods-applications.</Content>
                          {resStatus?.mlflow.ready && resStatus.mlflow.currentImage?.includes("odh-pr-") && (
                            <Label isCompact color="blue" className="pf-v6-u-mt-xs">
                              PR #{resStatus.mlflow.currentImage.match(/odh-pr-(\d+)/)?.[1]}
                            </Label>
                          )}
                          {resStatus?.mlflow.ready && (
                            <Flex alignItems={{ default: "alignItemsFlexEnd" }} gap={{ default: "gapSm" }} className="pf-v6-u-mt-sm">
                              <FlexItem>
                                <Content component="small" className="pf-v6-u-mb-xs">PR (opendatahub-io/mlflow)</Content>
                                <TextInput type="number" value={mlflowPR} onChange={(_e, val) => setMlflowPR(val)} placeholder="e.g. 42" aria-label="MLflow PR number" className="pf-v6-u-w-initial" />
                              </FlexItem>
                              <FlexItem>
                                <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                  <Button variant="primary" size="sm" onClick={() => setSetupConfirm("deploy-mlflow-pr")} isDisabled={!canMutate || !mlflowPR || !Number.isInteger(Number(mlflowPR)) || Number(mlflowPR) <= 0 || !!resAction} isLoading={resAction === "deploy-mlflow-pr"}>Deploy PR</Button>
                                </Tooltip>
                              </FlexItem>
                              {resStatus.mlflow.currentImage?.includes("odh-pr-") && (
                                <FlexItem>
                                  <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                    <Button variant="secondary" size="sm" onClick={() => setSetupConfirm("revert-mlflow")} isDisabled={!canMutate || !!resAction} isLoading={resAction === "revert-mlflow"}>Revert</Button>
                                  </Tooltip>
                                </FlexItem>
                              )}
                            </Flex>
                          )}
                        </DataListCell>,
                        <DataListCell key="status" width={1} alignRight>
                          <Flex justifyContent={{ default: "justifyContentFlexEnd" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                            {resStatus?.mlflow.ready ? (
                              <>
                                <FlexItem><Label isCompact color="green" icon={<CheckCircleIcon />}>Running</Label></FlexItem>
                                {resStatus.mlflow.uiRoute && (
                                  <FlexItem>
                                    <Button variant="link" isInline component="a" href={resStatus.mlflow.uiRoute} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">Open MLflow</Button>
                                  </FlexItem>
                                )}
                                <FlexItem>
                                  <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                    <Button variant="secondary" isDanger size="sm" onClick={() => setTeardownConfirm("mlflow")} isDisabled={!canMutate || !!resAction}>Tear down</Button>
                                  </Tooltip>
                                </FlexItem>
                              </>
                            ) : resStatus?.mlflow.deployed ? (
                              <FlexItem><Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Starting" />}>{resStatus.mlflow.message}</Label></FlexItem>
                            ) : (
                              <FlexItem>
                                <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                  <Button variant="primary" size="sm" onClick={() => setSetupConfirm("setup-mlflow")} isDisabled={!canMutate || !!resAction} isLoading={resAction === "setup-mlflow"}>Set up</Button>
                                </Tooltip>
                              </FlexItem>
                            )}
                          </Flex>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                </DataListItem>
              </DataList>
            </StackItem>

            {/* Pipelines */}
            <StackItem>
              <Content component="h4">Pipelines</Content>
              {!resStatus?.minio.ready && (
                <Alert variant="warning" title="Requires MinIO — set up Storage first" isInline isPlain className="pf-v6-u-mb-sm" />
              )}
              {(resStatus?.pipelineServers || []).length === 0 && resStatus?.minio.ready && (
                <Content component="small" className="pf-v6-u-mb-sm">No pipeline servers configured. Use the button below to add one to a project.</Content>
              )}
              <DataList isCompact aria-label="Pipeline servers">
                {(resStatus?.pipelineServers || []).map((ps) => (
                  <DataListItem key={ps.namespace} aria-labelledby={`ps-${ps.namespace}`}>
                    <DataListItemRow>
                      <DataListItemCells
                        dataListCells={[
                          <DataListCell key="name" width={2} id={`ps-${ps.namespace}`}>
                            <strong>{ps.namespace}</strong>
                          </DataListCell>,
                          <DataListCell key="status" width={1} alignRight>
                            <Flex justifyContent={{ default: "justifyContentFlexEnd" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
                              {ps.ready ? (
                                <>
                                  <FlexItem><Label isCompact color="green" icon={<CheckCircleIcon />}>Running</Label></FlexItem>
                                  {ps.uiRoute && (
                                    <FlexItem>
                                      <Button variant="link" isInline component="a" href={ps.uiRoute} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">Pipelines</Button>
                                    </FlexItem>
                                  )}
                                  <FlexItem>
                                    <Tooltip content="Another operation is in progress" trigger={resAction ? "mouseenter focus" : "manual"}>
                                      <Button variant="secondary" isDanger size="sm" onClick={() => setTeardownConfirm(ps.namespace!)} isDisabled={!canMutate || !!resAction}>Tear down</Button>
                                    </Tooltip>
                                  </FlexItem>
                                </>
                              ) : (
                                <FlexItem><Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Starting" />}>{ps.message}</Label></FlexItem>
                              )}
                            </Flex>
                          </DataListCell>,
                        ]}
                      />
                    </DataListItemRow>
                  </DataListItem>
                ))}
                <DataListItem aria-labelledby="add-pipeline-item">
                  <DataListItemRow>
                    <DataListItemCells
                      dataListCells={[
                        <DataListCell key="add" id="add-pipeline-item">
                          <Select
                  isOpen={addProjectOpen}
                  onOpenChange={setAddProjectOpen}
                  onSelect={(_e, val) => {
                    setAddProjectOpen(false);
                    setSetupConfirmProject(val as string);
                  }}
                  toggle={(toggleRef: React.Ref<MenuToggleElement>) => (
                    <MenuToggle ref={toggleRef} onClick={() => setAddProjectOpen(!addProjectOpen)} isExpanded={addProjectOpen} isDisabled={!canMutate || !resStatus?.minio.ready || !!resAction} variant="primary">
                      {resAction === "setup-pipeline-server" ? <><Spinner size="sm" aria-label="Setting up" /> Setting up...</> : "+ Add to project"}
                    </MenuToggle>
                  )}
                >
                  <SelectList aria-label="Data Science projects">
                    {availableProjects.map((p) => (
                      <SelectOption key={p} value={p}>{p}</SelectOption>
                    ))}
                    {availableProjects.length === 0 && (
                      <SelectOption isDisabled value="">All projects already have pipeline servers</SelectOption>
                    )}
                  </SelectList>
                          </Select>
                        </DataListCell>,
                      ]}
                    />
                  </DataListItemRow>
                </DataListItem>
              </DataList>
            </StackItem>
          </Stack>
        </CardBody>
      </Card>

      {/* Teardown Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-teardown-title"
        variant={ModalVariant.small}
        isOpen={!!teardownConfirm}
        onClose={() => setTeardownConfirm(null)}
      >
        <ModalHeader
          title={`Tear down ${getTeardownLabel(teardownConfirm)}?`}
          labelId="confirm-teardown-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Alert variant="warning" title="Shared cluster impact" isInline>
                {getTeardownWarning(teardownConfirm)}
              </Alert>
            </StackItem>
            {teardownConfirm === "minio" && (resStatus?.pipelineServers?.length ?? 0) > 0 && (
              <StackItem>
                <Alert variant="danger" title="Pipeline servers depend on MinIO" isInline>
                  Pipeline servers are running in: {(resStatus?.pipelineServers || []).map((ps) => ps.namespace).join(", ")}. Tear them down first, or the backend will block this operation.
                </Alert>
              </StackItem>
            )}
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="danger"
            onClick={() => {
              executeTeardown(teardownConfirm!);
            }}
            isLoading={!!resAction}
            isDisabled={!!resAction || (teardownConfirm === "minio" && (resStatus?.pipelineServers?.length ?? 0) > 0)}
          >
            Tear down
          </Button>
          <Button variant="link" onClick={() => setTeardownConfirm(null)} isDisabled={!!resAction}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>

      {/* Pipeline Server Setup Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-setup-pipeline-title"
        variant={ModalVariant.small}
        isOpen={!!setupConfirmProject}
        onClose={() => setSetupConfirmProject(null)}
      >
        <ModalHeader
          title={`Set up Pipeline Server in ${setupConfirmProject}?`}
          labelId="confirm-setup-pipeline-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="small">This will create a DSPA (DataSciencePipelinesApplication) and S3 credentials in project <strong>{setupConfirmProject}</strong>, backed by MinIO storage.</Content>
            </StackItem>
            <StackItem>
              <Alert variant="info" title="The pipeline server will take 1-3 minutes to become ready." isInline isPlain />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => {
              const project = setupConfirmProject!;
              setSetupConfirmProject(null);
              handleResourceAction("setup-pipeline-server", () => setupPipelineServer(project));
            }}
            isLoading={!!resAction}
            isDisabled={!!resAction}
          >
            Set up
          </Button>
          <Button variant="link" onClick={() => setSetupConfirmProject(null)} isDisabled={!!resAction}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>

      {/* Setup / Action Confirmation Modal */}
      <Modal
        aria-labelledby="confirm-setup-title"
        variant={ModalVariant.small}
        isOpen={!!setupConfirm}
        onClose={() => setSetupConfirm(null)}
      >
        <ModalHeader
          title={`${getSetupLabel(setupConfirm)}?`}
          labelId="confirm-setup-title"
        />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>
              <Alert variant="warning" title="This action will modify resources on the shared cluster" isInline>
                {getSetupWarning(setupConfirm)}
              </Alert>
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => executeSetup(setupConfirm!)}
            isLoading={!!resAction}
            isDisabled={!!resAction}
          >
            {getSetupLabel(setupConfirm)}
          </Button>
          <Button variant="link" onClick={() => setSetupConfirm(null)} isDisabled={!!resAction}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </>
  );
};
