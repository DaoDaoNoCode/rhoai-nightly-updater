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
  InputGroupText,
  Label,
  Stack,
  StackItem,
  TextInput,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import EyeIcon from "@patternfly/react-icons/dist/esm/icons/eye-icon";
import EyeSlashIcon from "@patternfly/react-icons/dist/esm/icons/eye-slash-icon";
import KeyIcon from "@patternfly/react-icons/dist/esm/icons/key-icon";
import type { OperationResponse, PullSecretInfo } from "../types";
import { createPullSecret, testPullSecret } from "../services/api";

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

function existsLabel(exists: boolean) {
  return exists ? (
    <Label color="green" icon={<CheckCircleIcon />}>
      Ready
    </Label>
  ) : (
    <Label color="red" icon={<ExclamationCircleIcon />}>
      Missing
    </Label>
  );
}

function cardStatusIcon(ok: boolean) {
  return (
    <Icon status={ok ? "success" : "danger"}>
      {ok ? <CheckCircleIcon /> : <ExclamationCircleIcon />}
    </Icon>
  );
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
      const msg = e instanceof Error ? e.message : "Failed";
      setAuthResult({
        success: false,
        message: msg.includes("403")
          ? "You don't have permission to create secrets in kube-system"
          : msg,
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
        message: e instanceof Error ? e.message : "Test failed",
        logs: [],
      });
    } finally {
      setTestLoading(false);
    }
  };

  return (
    <GridItem lg={3} md={6} sm={12}>
      <Card isFullHeight isCompact>
        <CardHeader>
          <CardTitle>
            <Flex
              alignItems={{ default: "alignItemsCenter" }}
              gap={{ default: "gapSm" }}
              flexWrap={{ default: "nowrap" }}
            >
              <FlexItem>
                <Icon>
                  <KeyIcon />
                </Icon>
              </FlexItem>
              <FlexItem>Pull Secret</FlexItem>
              <FlexItem>
                {cardStatusIcon(pullSecret.exists)}
              </FlexItem>
            </Flex>
          </CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            <StackItem>
              <Flex
                gap={{ default: "gapSm" }}
                alignItems={{ default: "alignItemsCenter" }}
              >
                <FlexItem>
                  {existsLabel(pullSecret.exists)}
                </FlexItem>
              </Flex>
            </StackItem>
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
                    <Alert
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
                          Get credentials from Bitwarden (Openshift AI
                          devel collection).
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
                      Get credentials from Bitwarden (Openshift AI devel
                      collection).
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
                <Alert
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
