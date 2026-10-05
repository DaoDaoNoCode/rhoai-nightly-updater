import React, { useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  GridItem,
  HelperText,
  HelperTextItem,
  Icon,
  InputGroup,
  InputGroupItem,
  Label,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import EyeIcon from "@patternfly/react-icons/dist/esm/icons/eye-icon";
import EyeSlashIcon from "@patternfly/react-icons/dist/esm/icons/eye-slash-icon";
import KeyIcon from "@patternfly/react-icons/dist/esm/icons/key-icon";
import type { OperationResponse, PullSecretInfo } from "../types";
import { createPullSecret, testPullSecret, toApiError } from "../services/api";
import { describeError } from "../errors";
import { sentence } from "../build";
import { TooltipButton } from "./TooltipButton";

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

interface PullSecretCardProps {
  pullSecret: PullSecretInfo;
  onStatusRefresh?: () => void;
  /** Why saving a secret is not possible now (permissions, another operation), or null. */
  disabledReason?: string | null;
}

const CREDENTIALS_HELP = (
  <>
    Get the base64 auth token for quay.io/rhoai from <a href="https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1" target="_blank" rel="noopener noreferrer">Bitwarden</a> (OpenShift
    AI devel collection). No access? Ask in <a href="https://redhat.enterprise.slack.com/archives/C07TF3MBMMW" target="_blank" rel="noopener noreferrer">#rhoai-devtestops-requests</a>.
  </>
);

export const PullSecretCard: React.FC<PullSecretCardProps> = ({
  pullSecret,
  onStatusRefresh,
  disabledReason = null,
}) => {
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
        <Content component="small">{CREDENTIALS_HELP}</Content>
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
            >
              {showSecret ? <EyeSlashIcon /> : <EyeIcon />}
            </Button>
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
          size="sm"
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
    <GridItem lg={6} md={6} sm={12}>
      <Card isFullHeight isCompact>
        <CardHeader>
          <CardTitle>
            <Flex
              alignItems={{ default: "alignItemsCenter" }}
              justifyContent={{ default: "justifyContentSpaceBetween" }}
              flexWrap={{ default: "nowrap" }}
            >
              <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                <FlexItem>
                  <Icon><KeyIcon /></Icon>
                </FlexItem>
                <FlexItem>Pull secret</FlexItem>
              </Flex>
              <FlexItem>
                {pullSecret.exists && pullSecret.valid
                  ? <Label color="green" variant="outline" isCompact>Ready</Label>
                  : pullSecret.exists
                    ? <Label status="warning" variant="outline" isCompact>Invalid</Label>
                    : <Label status="danger" variant="outline" isCompact>Missing</Label>}
              </FlexItem>
            </Flex>
          </CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            <StackItem>
              <Content component="small">Credentials the cluster uses to pull nightly images from quay.io/rhoai.</Content>
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
                  <Flex gap={{ default: "gapSm" }}>
                    <FlexItem>
                      <Button variant="secondary" size="sm" onClick={handleTest} isLoading={testLoading} isDisabled={testLoading}>
                        Test with Quay
                      </Button>
                    </FlexItem>
                    <FlexItem>
                      <Button variant="link" size="sm" onClick={() => setReplaceOpen(!replaceOpen)} aria-expanded={replaceOpen}>
                        {replaceOpen ? "Cancel replacing" : "Replace token"}
                      </Button>
                    </FlexItem>
                  </Flex>
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
        </CardBody>
      </Card>
    </GridItem>
  );
};
