import React, { useEffect, useRef, useState } from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  Button,
  Content,
  ExpandableSection,
  Flex,
  Form,
  FormGroup,
  FormHelperText,
  FlexItem,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import EyeIcon from "@patternfly/react-icons/dist/esm/icons/eye-icon";
import EyeSlashIcon from "@patternfly/react-icons/dist/esm/icons/eye-slash-icon";
import type { ClusterInfo, NodeSetupAction, OperationResponse, PullSecretInfo } from "../types";
import { NodeSetupDialog } from "./NodeSetupDialog";
import { checkNodePull, createPullSecret, testPullSecret, toApiError } from "../services/api";
import { CopyableCode } from "./environment/UseIt";
import { describeError } from "../errors";
import { sentence } from "../build";
import { TooltipButton } from "./TooltipButton";
import { StatusLabel } from "./StatusLabel";
import { useClusterBusyHandler, useMutationBlocker } from "../state/AppInfo";
import { errorResult } from "../outcomes";

export const isValidBase64Auth = (value: string): boolean => {
  const trimmed = value.trim();
  if (!trimmed) return false;
  try {
    const decoded = atob(trimmed);
    return decoded.includes(":");
  } catch {
    return false;
  }
};

interface PullSecretSetupProps {
  pullSecret: PullSecretInfo;
  onStatusRefresh?: () => void;
  /** id of the row title, for the row's aria-labelledby. */
  titleId?: string;
  /** State only, no test or replace (the gitops profile). */
  readOnly?: boolean;
  /** status.cluster.controlPlane: words the nodes' part (pullSecret.nodes). */
  controlPlane?: ClusterInfo["controlPlane"];
  /** The catalog image Check that the nodes can pull runs: the build Update would install. */
  checkImage?: string;
  /** Why there is no checkImage yet (the newest build is being looked up, or the lookup failed). */
  checkImageReason?: string;
  /** The last node check's result, when the parent keeps it (the setup card counts it). */
  nodeCheck?: OperationResponse | null;
  onNodeCheck?: (result: OperationResponse | null) => void;
}

/** The pull secret is saved and valid, and the nodes are known to have it (or use it). */
export function nodePullState(pullSecret: PullSecretInfo, nodeCheck: OperationResponse | null | undefined): "ready" | "unchecked" | "failed" | "not-applicable" {
  if (!pullSecret.nodes || !pullSecret.exists || !pullSecret.valid) return "not-applicable";
  if (pullSecret.nodes.pulled || nodeCheck?.success) return "ready";
  return nodeCheck && nodeCheck.errorCode === "catalog_image_pull" ? "failed" : "unchecked";
}

const NODE_PULL_RESULT_TITLE: Record<string, string> = {
  success: "The nodes can pull from quay.io/rhoai",
  catalog_image_pull: "The nodes cannot pull from quay.io/rhoai",
};

/**
 * The nodes' side of the pull secret on a cluster whose control plane runs
 * on the cluster: the nodes pull with openshift-config/pull-secret, which
 * the updater's ServiceAccount cannot read.
 *   - self-managed: Add to the nodes' pull secret (and its removal) copy the
 *     token saved above into it with the user's own permissions
 *     (NodeSetupDialog); the commands are under "Do it yourself instead";
 *   - managed (ROSA Classic, OSD): Red Hat creates that secret and documents
 *     no procedure for changing it, so there is no button; the commands are
 *     there for whoever chooses to;
 *   - unknown: the commands first, and the button as an attempt whose
 *     refusal the dialog explains.
 * Check that the nodes can pull runs a temporary catalog pod.
 */
const NodePullSecret: React.FC<{
  nodes: NonNullable<PullSecretInfo["nodes"]>;
  controlPlane?: ClusterInfo["controlPlane"];
  checkImage?: string;
  checkImageReason?: string;
  result: OperationResponse | null;
  onResult: (result: OperationResponse | null) => void;
  onStatusRefresh?: () => void;
}> = ({ nodes, controlPlane, checkImage, checkImageReason, result, onResult, onStatusRefresh }) => {
  const blocker = useMutationBlocker({ ignoreReconcile: true });
  const onBusy = useClusterBusyHandler();
  const [checking, setChecking] = useState(false);
  const [diyOpen, setDiyOpen] = useState(controlPlane === "managed");
  const [dialog, setDialog] = useState<NodeSetupAction | null>(null);
  const selfManaged = controlPlane === "self-managed";
  const managed = controlPlane === "managed";
  const unknown = controlPlane === undefined;

  const handleCheck = async () => {
    if (!checkImage) return;
    setChecking(true);
    onResult(null);
    try {
      onResult(await checkNodePull(checkImage));
    } catch (e) {
      onResult({ success: false, message: toApiError(e, "The check could not run").message, logs: [] });
      onBusy(errorResult(e, "The check could not run"));
    } finally {
      setChecking(false);
    }
  };

  const commands = (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">Add the quay.io/rhoai entry (as a cluster-admin, in bash or zsh with jq 1.6 or later):</Content>
      </StackItem>
      <StackItem>
        <CopyableCode id="node-pull-secret-command" label="the command" code={nodes.command} />
      </StackItem>
      <StackItem>
        <Content component="p" className="pf-v6-u-text-color-subtle">
          It asks for the token you saved above (base64 user:password, with its = padding) without showing it, and
          changes only the quay.io/rhoai entry. The nodes pick it up within a few minutes without a drain or reboot;{" "}
          <code>oc get mcp</code> shows UPDATED True for every pool when done.
        </Content>
      </StackItem>
      <StackItem>
        <Content component="p">Remove it again:</Content>
      </StackItem>
      <StackItem>
        <CopyableCode id="node-pull-secret-remove-command" label="the removal command" code={nodes.removeCommand} />
      </StackItem>
    </Stack>
  );

  const intro = nodes.pulled
    ? "The nodes pull with the cluster pull secret, openshift-config/pull-secret. The nightly catalog runs its quay.io/rhoai image, so they can pull."
    : selfManaged
      ? "This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret. The token saved above is the updater's own copy, for its Quay lookups. Add to the nodes' pull secret copies it there with your own permissions; every other registry login in that secret stays as it is."
      : managed
        ? "Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated): its nodes pull with the cluster pull secret, openshift-config/pull-secret. Red Hat creates that secret, and the documentation of these services has no procedure for changing it, so the updater does not change it. If you choose to add the quay.io/rhoai entry yourself, the commands are below."
        : "The updater could not read what kind of cluster this is. On ROSA HCP the nodes use the token saved above. If this cluster is self-managed, its nodes pull with the cluster pull secret, openshift-config/pull-secret: a cluster-admin adds the quay.io/rhoai entry with this command, or tries Add to the nodes' pull secret:";

  const checkReason = blocker ?? (checking ? null : checkImage ? null : checkImageReason ?? "Looking up the newest nightly build...");
  const setupButtons = !managed && (
    <>
      <ActionListItem>
        <TooltipButton variant={selfManaged && !nodes.pulled ? "primary" : "secondary"} onClick={() => setDialog("pull-secret-add")} disabledReason={blocker}>
          Add to the nodes&apos; pull secret
        </TooltipButton>
      </ActionListItem>
      {selfManaged && (
        <ActionListItem>
          <TooltipButton variant="secondary" isDanger onClick={() => setDialog("pull-secret-remove")} disabledReason={blocker}>
            Remove from the nodes&apos; pull secret
          </TooltipButton>
        </ActionListItem>
      )}
    </>
  );
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p">{intro}</Content>
      </StackItem>
      {unknown && <StackItem>{commands}</StackItem>}
      <StackItem>
        <ActionList>
          <ActionListGroup>
            {setupButtons}
            {!nodes.pulled && (
              <ActionListItem>
                <TooltipButton
                  variant="secondary"
                  onClick={handleCheck}
                  isLoading={checking || undefined}
                  isDisabled={checking}
                  disabledReason={checkReason}
                >
                  Check that the nodes can pull
                </TooltipButton>
              </ActionListItem>
            )}
          </ActionListGroup>
        </ActionList>
        {!nodes.pulled && (
          <Content component="p" className="pf-v6-u-text-color-subtle pf-v6-u-mt-sm">
            {checking
              ? "Waiting for a temporary catalog pod to pull the image. This takes up to two minutes."
              : "Check that the nodes can pull runs a temporary catalog pod with the newest nightly catalog image, then removes it. Nothing else changes."}
          </Content>
        )}
      </StackItem>
      {result && (
        <StackItem>
          <Alert
            component="p"
            variant={result.success ? "success" : result.errorCode === "catalog_image_pull" ? "danger" : "warning"}
            title={NODE_PULL_RESULT_TITLE[result.success ? "success" : result.errorCode ?? ""] ?? "The check found no answer"}
            isInline
            isPlain
            isLiveRegion
          >
            {result.message}
          </Alert>
        </StackItem>
      )}
      {!unknown && (
        <StackItem>
          <ExpandableSection
            toggleText={managed ? "The commands" : "Do it yourself instead"}
            isExpanded={diyOpen}
            onToggle={(_e, open) => setDiyOpen(open)}
            isIndented
          >
            {commands}
          </ExpandableSection>
        </StackItem>
      )}
      {dialog && (
        <NodeSetupDialog
          action={dialog}
          onClose={() => setDialog(null)}
          onChanged={() => {
            onResult(null);
            onStatusRefresh?.();
          }}
        />
      )}
    </Stack>
  );
};

const CREDENTIALS_HELP = (
  <>
    Get the base64 auth token for quay.io/rhoai from <a href="https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1" target="_blank" rel="noopener noreferrer">Bitwarden</a> (OpenShift
    AI devel collection). No access? Ask in <a href="https://redhat.enterprise.slack.com/archives/C07TF3MBMMW" target="_blank" rel="noopener noreferrer">#rhoai-devtestops-requests</a>.
  </>
);

/** The pull secret row of the cluster setup: its state, Test with Quay, and the create or replace form. */
export const PullSecretSetup: React.FC<PullSecretSetupProps> = ({
  pullSecret,
  onStatusRefresh,
  titleId = "pull-secret-title",
  readOnly = false,
  controlPlane,
  checkImage,
  checkImageReason,
  nodeCheck,
  onNodeCheck,
}) => {
  // The node check's result: the parent's when it keeps it, else this row's.
  const [ownNodeCheck, setOwnNodeCheck] = useState<OperationResponse | null>(null);
  const nodeResult = onNodeCheck ? nodeCheck ?? null : ownNodeCheck;
  const setNodeResult = onNodeCheck ?? setOwnNodeCheck;
  // The unified gate (permissions, session, this tab's stream, the backend
  // lock). Saving the secret repairs an install, so OLM still installing
  // after an operation does not block it.
  const disabledReason = useMutationBlocker({ ignoreReconcile: true });
  const onBusy = useClusterBusyHandler();
  const invalid = pullSecret.exists && !pullSecret.valid;
  // readOnly is the gitops profile, which does not use the secret
  // (utils.ts, pullSecretNotUsed).
  const notUsed = readOnly;
  // The nodes' part: after the updater's copy is saved, on a cluster whose
  // nodes may not use it (the backend leaves nodes out on a hosted control
  // plane and in the gitops profile).
  const nodes = notUsed ? undefined : pullSecret.nodes;
  const nodeState = notUsed ? "not-applicable" : nodePullState(pullSecret, nodeResult);
  const [authValue, setAuthValue] = useState("");
  const [authLoading, setAuthLoading] = useState(false);
  const [authResult, setAuthResult] = useState<OperationResponse | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [testResult, setTestResult] = useState<OperationResponse | null>(null);
  // An invalid secret has to be replaced, so the form starts open.
  const [replaceOpen, setReplaceOpen] = useState(invalid);
  const [showSecret, setShowSecret] = useState(false);
  const tokenInputRef = useRef<HTMLInputElement>(null);
  const replaceButtonRef = useRef<HTMLButtonElement>(null);
  // Focus follows the replace flow: into the token field when the form
  // opens, back to Replace token when it is cancelled (the button that
  // opened it was hidden meanwhile).
  const focusReplaceButton = useRef(false);
  const mounted = useRef(false);
  useEffect(() => {
    if (replaceOpen && mounted.current) tokenInputRef.current?.focus();
    if (!replaceOpen && focusReplaceButton.current) {
      focusReplaceButton.current = false;
      replaceButtonRef.current?.focus();
    }
    mounted.current = true;
  }, [replaceOpen]);
  const cancelReplace = () => {
    setAuthValue("");
    setShowSecret(false);
    focusReplaceButton.current = true;
    setReplaceOpen(false);
  };

  const handleSave = async () => {
    setAuthLoading(true);
    setAuthResult(null);
    try {
      const res = await createPullSecret(authValue);
      setAuthResult(res);
      if (res.success) {
        setAuthValue("");
        focusReplaceButton.current = true;
        setReplaceOpen(false);
        setTestResult(null);
        onStatusRefresh?.();
      }
    } catch (e) {
      const { title, body } = describeError(e, "Could not save the pull secret");
      setAuthResult({ success: false, message: `${title}: ${body}`, logs: [] });
      onBusy(errorResult(e, "Could not save the pull secret"));
    } finally {
      setAuthLoading(false);
    }
  };

  const handleTest = async () => {
    setTestLoading(true);
    setTestResult(null);
    try {
      setTestResult(await testPullSecret());
    } catch (e) {
      setTestResult({ success: false, message: toApiError(e, "The test could not run").message, logs: [] });
    } finally {
      setTestLoading(false);
    }
  };

  const tokenInvalid = !!authValue.trim() && !isValidBase64Auth(authValue);
  const saveReason = disabledReason
    ?? (!authValue.trim() ? "Paste the auth token first." : null)
    ?? (tokenInvalid ? "This is not a base64 user:password token." : null);

  // A standard PatternFly form: one labelled field, its helper text (where
  // to get the token, and where it is saved), then the actions. All spacing
  // comes from the Form's own tokens. For an existing secret it is the
  // confirmation step of Replace token; replacing overwrites a credential
  // that may work, so its action is danger.
  const fieldId = pullSecret.exists ? "update-pull-secret" : "create-pull-secret";
  const form = (
    <Form onSubmit={(e) => e.preventDefault()}>
      <FormGroup label="Auth token for quay.io/rhoai" fieldId={fieldId} isRequired>
        <InputGroup>
          <InputGroupItem isFill>
            <TextInput
              ref={tokenInputRef}
              id={fieldId}
              type={showSecret ? "text" : "password"}
              value={authValue}
              onChange={(_e, val) => setAuthValue(val)}
              placeholder="base64 user:password"
              isDisabled={authLoading}
              isRequired
              validated={tokenInvalid ? "error" : "default"}
              aria-describedby="pull-secret-token-help"
              autoComplete="off"
            />
          </InputGroupItem>
          <InputGroupItem>
            <Button
              variant="control"
              onClick={() => setShowSecret(!showSecret)}
              aria-label={showSecret ? "Hide token" : "Show token"}
              icon={showSecret ? <EyeSlashIcon /> : <EyeIcon />}
            />
          </InputGroupItem>
        </InputGroup>
        <FormHelperText>
          <HelperText id="pull-secret-token-help">
            {tokenInvalid ? (
              <HelperTextItem variant="error">Expected base64 of user:password (the &quot;auth&quot; value of a Docker config).</HelperTextItem>
            ) : (
              <HelperTextItem>
                {CREDENTIALS_HELP} Saved as additional-pull-secret in kube-system.
                {nodes && !pullSecret.exists && (controlPlane === "self-managed"
                  ? " The nodes of this self-managed cluster need it in openshift-config/pull-secret too: once it is saved, this item offers to add it there."
                  : controlPlane === "managed"
                    ? " The nodes of this cluster pull with openshift-config/pull-secret, which Red Hat manages: once it is saved, this item explains what that means."
                    : " If this cluster is self-managed, its nodes need it in openshift-config/pull-secret too: once it is saved, this item shows how to add it there.")}
              </HelperTextItem>
            )}
          </HelperText>
        </FormHelperText>
      </FormGroup>
      {/* An ActionList, not an ActionGroup: in this one-field form the
          buttons sit one form row gap below the helper text, without the
          action group's extra top margin. */}
      <ActionList>
        <ActionListGroup>
          <ActionListItem>
            <TooltipButton
              variant={pullSecret.exists ? "danger" : "primary"}
              onClick={handleSave}
              isLoading={authLoading || undefined}
              isDisabled={authLoading}
              disabledReason={saveReason}
            >
              {pullSecret.exists ? "Replace token" : "Create secret"}
            </TooltipButton>
          </ActionListItem>
          {pullSecret.exists && (
            <ActionListItem>
              <Button variant="link" onClick={cancelReplace} isDisabled={authLoading}>Cancel</Button>
            </ActionListItem>
          )}
        </ActionListGroup>
      </ActionList>
    </Form>
  );

  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><strong id={titleId}>Pull secret</strong></FlexItem>
          <FlexItem>
            {notUsed
              ? <StatusLabel status="neutral">Not used</StatusLabel>
              : nodeState === "unchecked"
              ? <StatusLabel status="warning">Nodes not checked</StatusLabel>
              : nodeState === "failed"
              ? <StatusLabel status="danger">Nodes can&apos;t pull</StatusLabel>
              : pullSecret.exists && pullSecret.valid
              ? <StatusLabel status="success">Ready</StatusLabel>
              : pullSecret.exists
                ? <StatusLabel status="warning">Invalid</StatusLabel>
                : <StatusLabel status="danger">Missing</StatusLabel>}
          </FlexItem>
        </Flex>
        <Content component="p" className="pf-v6-u-text-color-subtle">
          {notUsed
            ? "The gitops profile does not use additional-pull-secret: the cluster's pull secret is used. Git or ACM manage it here; the updater does not read or check it."
            : nodes
              ? "Credentials for quay.io/rhoai: the updater's copy here, and the cluster pull secret the nodes use."
              : "Credentials the cluster uses to pull nightly images from quay.io/rhoai."}
        </Content>
      </StackItem>

      {invalid && !notUsed && (
        <StackItem>
          <Alert variant="warning" isInline isPlain component="p" title="The cluster can't pull nightly images with this secret">
            {sentence(pullSecret.detail || "The secret has no usable quay.io/rhoai entry")} {readOnly ? "Replace it in Git." : "Replace the token below."}
          </Alert>
        </StackItem>
      )}

      {readOnly ? null : pullSecret.exists ? (
        <>
          <StackItem>
            {/* One group: related actions get PatternFly's action-to-action gap (md), not the group-to-group gap (2xl). */}
            <ActionList>
              <ActionListGroup>
                <ActionListItem>
                  <Button variant="secondary" onClick={handleTest} isLoading={testLoading} isDisabled={testLoading}>
                    Test with Quay
                  </Button>
                </ActionListItem>
                {/* Danger: replacing overwrites a credential that may work. It
                    opens the form below (the confirmation) and is hidden
                    while the form is open; Cancel there brings it back. */}
                {!replaceOpen && (
                  <ActionListItem>
                    <Button ref={replaceButtonRef} variant="secondary" isDanger onClick={() => setReplaceOpen(true)}>
                      Replace token
                    </Button>
                  </ActionListItem>
                )}
              </ActionListGroup>
            </ActionList>
          </StackItem>
          {testResult && (
            <StackItem>
              <Alert
                component="p"
                variant={testResult.success ? "success" : "danger"}
                title={testResult.success ? "Quay accepts the pull secret" : "Quay rejected the pull secret"}
                isInline
                isPlain
                isLiveRegion
              >
                {testResult.message}
              </Alert>
            </StackItem>
          )}
          {replaceOpen && <StackItem>{form}</StackItem>}
        </>
      ) : (
        <StackItem>{form}</StackItem>
      )}

      {authResult && (
        <StackItem>
          <Alert
            component="p"
            variant={authResult.success ? "success" : "danger"}
            title={authResult.message}
            isInline
            isPlain
            isLiveRegion
          />
        </StackItem>
      )}

      {nodes && pullSecret.exists && pullSecret.valid && (
        <StackItem>
          <NodePullSecret
            nodes={nodes}
            controlPlane={controlPlane}
            checkImage={checkImage}
            checkImageReason={checkImageReason}
            result={nodeResult}
            onResult={setNodeResult}
            onStatusRefresh={onStatusRefresh}
          />
        </StackItem>
      )}
    </Stack>
  );
};
