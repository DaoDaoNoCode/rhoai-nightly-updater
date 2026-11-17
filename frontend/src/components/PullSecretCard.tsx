import React, { useEffect, useRef, useState } from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Alert,
  Button,
  Content,
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
  /** State only, no test or replace (the gitops profile). */
  readOnly?: boolean;
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
  readOnly = false,
}) => {
  // The unified gate (permissions, session, this tab's stream, the backend
  // lock). Saving the secret repairs an install, so OLM still installing
  // after an operation does not block it.
  const disabledReason = useMutationBlocker({ ignoreReconcile: true });
  const onBusy = useClusterBusyHandler();
  const invalid = pullSecret.exists && !pullSecret.valid;
  // readOnly is the gitops profile, which does not use the secret
  // (utils.ts, pullSecretNotUsed).
  const notUsed = readOnly;
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
              <HelperTextItem>{CREDENTIALS_HELP} Saved as additional-pull-secret in kube-system.</HelperTextItem>
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
    </Stack>
  );
};
