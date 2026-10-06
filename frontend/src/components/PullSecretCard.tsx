import React, { useState } from "react";
import {
  ActionList,
  ActionListItem,
  Alert,
  Button,
  Content,
  Flex,
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
import type { OperationResponse, PullSecretInfo } from "../types";
import { createPullSecret, testPullSecret, toApiError } from "../services/api";
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
}

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
}) => {
  // The unified gate (permissions, session, this tab's stream, the backend
  // lock). Saving the secret repairs an install, so OLM still installing
  // after an operation does not block it.
  const disabledReason = useMutationBlocker({ ignoreReconcile: true });
  const onBusy = useClusterBusyHandler();
  const invalid = pullSecret.exists && !pullSecret.valid;
  const [authValue, setAuthValue] = useState("");
  const [authLoading, setAuthLoading] = useState(false);
  const [authResult, setAuthResult] = useState<OperationResponse | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [testResult, setTestResult] = useState<OperationResponse | null>(null);
  // An invalid secret has to be replaced, so the form starts open.
  const [replaceOpen, setReplaceOpen] = useState(invalid);
  const [showSecret, setShowSecret] = useState(false);

  const handleSave = async () => {
    setAuthLoading(true);
    setAuthResult(null);
    try {
      const res = await createPullSecret(authValue);
      setAuthResult(res);
      if (res.success) {
        setAuthValue("");
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

  const form = (
    <Stack hasGutter>
      <StackItem>
        <Content component="p" className="pf-v6-u-font-size-sm">{CREDENTIALS_HELP}</Content>
      </StackItem>
      <StackItem>
        <InputGroup>
          <InputGroupItem isFill>
            <TextInput
              id={pullSecret.exists ? "update-pull-secret" : "create-pull-secret"}
              type={showSecret ? "text" : "password"}
              value={authValue}
              onChange={(_e, val) => setAuthValue(val)}
              placeholder="base64 auth token"
              isDisabled={authLoading}
              aria-label="quay.io/rhoai auth token"
              validated={tokenInvalid ? "error" : "default"}
              aria-describedby="pull-secret-token-help"
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
        <HelperText id="pull-secret-token-help">
          <HelperTextItem variant={tokenInvalid ? "error" : "default"}>
            {tokenInvalid ? "Expected base64 of user:password (the \"auth\" value of a Docker config)." : "Saved as additional-pull-secret in kube-system."}
          </HelperTextItem>
        </HelperText>
      </StackItem>
      <StackItem>
        <TooltipButton
          variant={pullSecret.exists ? "secondary" : "primary"}
          onClick={handleSave}
          isLoading={authLoading}
          isDisabled={authLoading}
          disabledReason={saveReason}
        >
          {pullSecret.exists ? "Replace secret" : "Create secret"}
        </TooltipButton>
      </StackItem>
    </Stack>
  );

  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
          <FlexItem><strong id={titleId}>Pull secret</strong></FlexItem>
          <FlexItem>
            {pullSecret.exists && pullSecret.valid
              ? <StatusLabel status="success">Ready</StatusLabel>
              : pullSecret.exists
                ? <StatusLabel status="warning">Invalid</StatusLabel>
                : <StatusLabel status="danger">Missing</StatusLabel>}
          </FlexItem>
        </Flex>
        <Content component="p" className="pf-v6-u-text-color-subtle">Credentials the cluster uses to pull nightly images from quay.io/rhoai.</Content>
      </StackItem>

      {invalid && (
        <StackItem>
          <Alert variant="warning" isInline isPlain component="p" title="The cluster can't pull nightly images with this secret">
            {sentence(pullSecret.detail || "The secret has no usable quay.io/rhoai entry")} Replace the token below.
          </Alert>
        </StackItem>
      )}

      {pullSecret.exists ? (
        <>
          <StackItem>
            <ActionList>
              <ActionListItem>
                <Button variant="secondary" onClick={handleTest} isLoading={testLoading} isDisabled={testLoading}>
                  Test with Quay
                </Button>
              </ActionListItem>
              <ActionListItem>
                <Button variant="link" isInline onClick={() => setReplaceOpen(!replaceOpen)} aria-expanded={replaceOpen}>
                  {replaceOpen ? "Cancel replacing" : "Replace token"}
                </Button>
              </ActionListItem>
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
    </Stack>
  );
};
