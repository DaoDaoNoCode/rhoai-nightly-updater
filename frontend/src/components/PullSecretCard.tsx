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

const isValidBase64Auth = (value: string): boolean => {
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
  canMutate?: boolean;
}

export const PullSecretCard: React.FC<PullSecretCardProps> = ({
  pullSecret,
  onStatusRefresh,
  canMutate = true,
}) => {
  const [authValue, setAuthValue] = useState("");
  const [authLoading, setAuthLoading] = useState(false);
  const [authResult, setAuthResult] = useState<OperationResponse | null>(null);
  const [testLoading, setTestLoading] = useState(false);
  const [testResult, setTestResult] = useState<OperationResponse | null>(null);
  const [updateExpanded, setUpdateExpanded] = useState(false);
  const [showSecret, setShowSecret] = useState(false);

  const handleCreatePullSecret = async () => {
    setAuthLoading(true);
    setAuthResult(null);
    try {
      const res = await createPullSecret(authValue);
      setAuthResult(res);
      if (res.success) {
        setAuthValue("");
        setUpdateExpanded(false);
        onStatusRefresh?.();
      }
    } catch (e) {
      const err = toApiError(e, "Failed to save the pull secret");
      setAuthResult({
        success: false,
        message: err.status === 403
          ? "You don't have permission to create secrets in kube-system"
          : err.message,
        logs: [],
      });
    } finally {
      setAuthLoading(false);
    }
  };

  const handleTestPullSecret = async () => {
    setTestLoading(true);
    setTestResult(null);
    try {
      const res = await testPullSecret();
      setTestResult(res);
    } catch (e) {
      setTestResult({
        success: false,
        message: toApiError(e, "Test failed").message,
        logs: [],
      });
    } finally {
      setTestLoading(false);
    }
  };

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
                  <Icon>
                    <KeyIcon />
                  </Icon>
                </FlexItem>
                <FlexItem>Pull Secret</FlexItem>
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
              <Content component="small">
                Pull credentials for quay.io/rhoai nightly images
              </Content>
            </StackItem>

            {pullSecret.exists ? (
              <>
                <StackItem>
                  <Flex gap={{ default: "gapSm" }}>
                    <FlexItem>
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={handleTestPullSecret}
                        isLoading={testLoading}
                        isDisabled={testLoading}
                      >
                        Test
                      </Button>
                    </FlexItem>
                    <FlexItem>
                      <Button
                        variant="link"
                        size="sm"
                        onClick={() => setUpdateExpanded(!updateExpanded)}
                      >
                        Update
                      </Button>
                    </FlexItem>
                  </Flex>
                </StackItem>
                {testResult && (
                  <StackItem>
                    <Alert component="p"
                      variant={testResult.success ? "success" : "danger"}
                      title={testResult.success ? "Valid" : "Invalid"}
                      isInline
                      isPlain
                    />
                  </StackItem>
                )}
                {updateExpanded && (
                  <StackItem>
                    <Stack hasGutter>
                      <StackItem>
                        <Content component="small">
                          Get credentials from <a href="https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1" target="_blank" rel="noopener noreferrer">Bitwarden</a> (Openshift AI
                          devel collection). No access? Request in <a href="https://redhat.enterprise.slack.com/archives/C07TF3MBMMW" target="_blank" rel="noopener noreferrer">#rhoai-devtestops-requests</a>.
                        </Content>
                      </StackItem>
                      <StackItem>
                        <InputGroup>
                          <InputGroupItem isFill>
                            <TextInput
                              id="update-pull-secret"
                              type={showSecret ? "text" : "password"}
                              value={authValue}
                              onChange={(_e, val) => setAuthValue(val)}
                              placeholder="base64 auth token"
                              isDisabled={authLoading}
                              aria-label="Auth token"
                            />
                          </InputGroupItem>
                          <InputGroupItem>
                            <Button
                              variant="control"
                              onClick={() => setShowSecret(!showSecret)}
                              aria-label={showSecret ? "Hide secret" : "Show secret"}
                            >
                              {showSecret ? <EyeSlashIcon /> : <EyeIcon />}
                            </Button>
                          </InputGroupItem>
                        </InputGroup>
                      </StackItem>
                      <StackItem>
                        <Button
                          variant="secondary"
                          size="sm"
                          onClick={handleCreatePullSecret}
                          isLoading={authLoading}
                          isDisabled={authLoading || !isValidBase64Auth(authValue) || !canMutate}
                        >
                          Update Secret
                        </Button>
                      </StackItem>
                    </Stack>
                  </StackItem>
                )}
              </>
            ) : (
              <StackItem>
                <Stack hasGutter>
                  <StackItem>
                    <Content component="small">
                      Get credentials from <a href="https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1" target="_blank" rel="noopener noreferrer">Bitwarden</a> (Openshift AI devel
                      collection). No access? Request in <a href="https://redhat.enterprise.slack.com/archives/C07TF3MBMMW" target="_blank" rel="noopener noreferrer">#rhoai-devtestops-requests</a>.
                    </Content>
                  </StackItem>
                  <StackItem>
                    <InputGroup>
                      <InputGroupItem isFill>
                        <TextInput
                          id="create-pull-secret"
                          type={showSecret ? "text" : "password"}
                          value={authValue}
                          onChange={(_e, val) => setAuthValue(val)}
                          placeholder="base64 auth token"
                          isDisabled={authLoading}
                          aria-label="quay.io/rhoai auth token"
                        />
                      </InputGroupItem>
                      <InputGroupItem>
                        <Button
                          variant="control"
                          onClick={() => setShowSecret(!showSecret)}
                          aria-label={showSecret ? "Hide secret" : "Show secret"}
                        >
                          {showSecret ? <EyeSlashIcon /> : <EyeIcon />}
                        </Button>
                      </InputGroupItem>
                    </InputGroup>
                  </StackItem>
                  <StackItem>
                    <Button
                      variant="primary"
                      size="sm"
                      onClick={handleCreatePullSecret}
                      isLoading={authLoading}
                      isDisabled={authLoading || !isValidBase64Auth(authValue) || !canMutate}
                    >
                      Create Secret
                    </Button>
                  </StackItem>
                </Stack>
              </StackItem>
            )}

            {authResult && (
              <StackItem>
                <Alert component="p"
                  variant={authResult.success ? "success" : "danger"}
                  title={authResult.message}
                  isInline
                  isPlain
                />
              </StackItem>
            )}
          </Stack>
        </CardBody>
      </Card>
    </GridItem>
  );
};
