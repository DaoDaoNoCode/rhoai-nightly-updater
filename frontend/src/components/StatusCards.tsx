import React, { useState } from "react";
import {
  Alert,
  Bullseye,
  Button,
  Card,
  CardHeader,
  CardTitle,
  CardBody,
  Content,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  ExpandableSection,
  Flex,
  FlexItem,
  Grid,
  GridItem,
  HelperText,
  HelperTextItem,
  Icon,
  Label,
  List,
  ListItem,
  Spinner,
  Stack,
  StackItem,
  Tooltip,
} from "@patternfly/react-core";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import ExclamationTriangleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-triangle-icon";
import CubesIcon from "@patternfly/react-icons/dist/esm/icons/cubes-icon";
import CatalogIcon from "@patternfly/react-icons/dist/esm/icons/catalog-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import CloneIcon from "@patternfly/react-icons/dist/esm/icons/clone-icon";
import { Link } from "react-router-dom";
import { truncateImage } from "../utils";
import type { StatusResponse } from "../types";
import { prerequisitesMet as checkPrereqs } from "../utils";
import { PullSecretCard } from "./PullSecretCard";

interface StatusCardsProps {
  status: StatusResponse | null;
  loading: boolean;
  error: string | null;
  reconciling?: boolean;
  onStatusRefresh?: () => void;
  canMutate?: boolean;
}

function phaseLabel(phase: string) {
  switch (phase) {
    case "Succeeded":
      return (
        <Label color="green" icon={<CheckCircleIcon />}>
          {phase}
        </Label>
      );
    case "Failed":
      return (
        <Label color="red" icon={<ExclamationCircleIcon />}>
          {phase}
        </Label>
      );
    case "Not Found":
      return <Label color="grey">Not Installed</Label>;
    case "Installing":
    case "Replacing":
      return (
        <Label
          color="blue"
          icon={<Spinner size="sm" aria-label="In progress" />}
        >
          {phase}
        </Label>
      );
    default:
      return (
        <Label color="orange" icon={<ExclamationTriangleIcon />}>
          {phase || "Unknown"}
        </Label>
      );
  }
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

export const StatusCards: React.FC<StatusCardsProps> = ({
  status,
  loading,
  error,
  reconciling,
  onStatusRefresh,
  canMutate = true,
}) => {
  const [setupExpanded, setSetupExpanded] = useState(false);

  if (loading && !status) {
    return (
      <Bullseye role="status">
        <Flex
          alignItems={{ default: "alignItemsCenter" }}
          gap={{ default: "gapSm" }}
        >
          <FlexItem>
            <Spinner size="lg" aria-label="Loading cluster status" />
          </FlexItem>
          <FlexItem>Loading cluster status...</FlexItem>
        </Flex>
      </Bullseye>
    );
  }

  if (error) {
    return (
      <Alert variant="danger" title="Failed to load status" isInline>
        {error}
      </Alert>
    );
  }

  if (!status) return null;

  const safeConsoleURL = status.consoleURL?.startsWith("https://")
    ? status.consoleURL
    : "";

  const errors = status.errors;
  const csvOk = status.csv.phase === "Succeeded";
  const catalogTransient =
    status.catalogSource.exists &&
    status.catalogSource.state === "TRANSIENT_FAILURE";
  const catalogOk = status.catalogSource.exists && !catalogTransient;
  const setupDone = checkPrereqs(status);

  return (
    <Stack hasGutter>
      {reconciling && (
        <StackItem>
          <Alert
            variant="info"
            title="Reconciling... See progress below."
            isInline
            isPlain
          />
        </StackItem>
      )}

      {errors && errors.length > 0 && (
        <StackItem>
          <Alert
            variant="warning"
            title="Some status checks reported errors"
            isInline
          >
            <List>
              {errors.map((err, i) => (
                <ListItem key={i}>{err}</ListItem>
              ))}
            </List>
          </Alert>
        </StackItem>
      )}

      <StackItem>
        <Grid hasGutter>
          {/* --- Operator Card --- */}
          <GridItem lg={6} md={6} sm={12}>
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
                        <CubesIcon />
                      </Icon>
                    </FlexItem>
                    <FlexItem>
                      <Link
                        to="/components"
                        style={{ color: "inherit", textDecoration: "none" }}
                      >
                        RHOAI Operator
                      </Link>
                    </FlexItem>
                    {status.csv.phase !== "Not Found" && status.csv.phase !== "Installing" && status.csv.phase !== "Replacing" && (
                      <FlexItem>{cardStatusIcon(csvOk)}</FlexItem>
                    )}
                  </Flex>
                </CardTitle>
              </CardHeader>
              <CardBody>
                <DescriptionList isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Version</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status.csv.name || "N/A"}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Phase</DescriptionListTerm>
                    <DescriptionListDescription>
                      {phaseLabel(status.csv.phase)}
                      {status.csv.phase === "Failed" && (
                        <>
                          <HelperText>
                            <HelperTextItem variant="error">
                              Check the operator pod logs for details.
                            </HelperTextItem>
                          </HelperText>
                          {safeConsoleURL && (
                            <Button
                              variant="link"
                              size="sm"
                              component="a"
                              href={`${safeConsoleURL}/k8s/ns/redhat-ods-operator/operators.coreos.com~v1alpha1~ClusterServiceVersion`}
                              target="_blank"
                              rel="noopener noreferrer"
                              icon={<ExternalLinkAltIcon />}
                              iconPosition="end"
                            >
                              Debug in console
                            </Button>
                          )}
                        </>
                      )}
                      {status.csv.phase === "Installing" && safeConsoleURL && (
                        <Button
                          variant="link"
                          size="sm"
                          component="a"
                          href={`${safeConsoleURL}/k8s/ns/redhat-ods-operator/operators.coreos.com~v1alpha1~ClusterServiceVersion`}
                          target="_blank"
                          rel="noopener noreferrer"
                          icon={<ExternalLinkAltIcon />}
                          iconPosition="end"
                        >
                          View in console
                        </Button>
                      )}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Cluster</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status.cluster.version}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
                <Link
                  to="/components"
                  style={{
                    display: "block",
                    marginTop: "0.75rem",
                    fontSize: "0.875rem",
                  }}
                >
                  View all &rarr;
                </Link>
              </CardBody>
            </Card>
          </GridItem>

          {/* --- Catalog Source Card --- */}
          <GridItem lg={6} md={6} sm={12}>
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
                        <CatalogIcon />
                      </Icon>
                    </FlexItem>
                    <FlexItem>Catalog Source</FlexItem>
                    <FlexItem>
                      {status.catalogSource.exists ? (
                        catalogTransient ? (
                          <Label isCompact color="blue">
                            <Spinner size="sm" aria-label="Loading" /> Loading
                          </Label>
                        ) : (
                          cardStatusIcon(catalogOk)
                        )
                      ) : (
                        <Label isCompact color="grey">
                          Inactive
                        </Label>
                      )}
                    </FlexItem>
                  </Flex>
                </CardTitle>
              </CardHeader>
              <CardBody>
                <DescriptionList isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Source</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status.subscription.source || (
                        <Content component="small">Not configured</Content>
                      )}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Channel</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status.subscription.channel || (
                        <Content component="small">Not configured</Content>
                      )}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  {catalogTransient && (
                    <DescriptionListGroup>
                      <DescriptionListTerm>Status</DescriptionListTerm>
                      <DescriptionListDescription>
                        <HelperText>
                          <HelperTextItem variant="default">
                            Catalog is loading — this is normal and usually
                            resolves in 1-2 minutes.
                          </HelperTextItem>
                        </HelperText>
                        {safeConsoleURL && (
                          <Button
                            variant="link"
                            size="sm"
                            component="a"
                            href={`${safeConsoleURL}/k8s/ns/openshift-marketplace/operators.coreos.com~v1alpha1~CatalogSource/${status.catalogSource.name}`}
                            target="_blank"
                            rel="noopener noreferrer"
                            icon={<ExternalLinkAltIcon />}
                            iconPosition="end"
                          >
                            View in console
                          </Button>
                        )}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  )}
                  {status.catalogSource.exists &&
                    status.catalogSource.image && (
                      <DescriptionListGroup>
                        <DescriptionListTerm>Image</DescriptionListTerm>
                        <DescriptionListDescription>
                          <Content component="small">
                            <Tooltip content={status.catalogSource.image}>
                              <code>
                                {truncateImage(status.catalogSource.image)}
                              </code>
                            </Tooltip>
                          </Content>
                        </DescriptionListDescription>
                      </DescriptionListGroup>
                    )}
                </DescriptionList>
              </CardBody>
            </Card>
          </GridItem>
        </Grid>
      </StackItem>
      {setupDone && (
        <StackItem>
          <ExpandableSection
            toggleText={setupExpanded ? "Hide cluster setup" : "Cluster setup"}
            onToggle={(_e, expanded) => setSetupExpanded(expanded)}
            isExpanded={setupExpanded}
          >
            <Grid hasGutter>
              <PullSecretCard
                pullSecret={status.pullSecret}
                canMutate={canMutate}
                onStatusRefresh={onStatusRefresh}
              />
              <GridItem lg={6} md={6} sm={12}>
                <Card isFullHeight isCompact>
                  <CardHeader>
                    <CardTitle>
                      <Flex alignItems={{ default: "alignItemsCenter" }} justifyContent={{ default: "justifyContentSpaceBetween" }} flexWrap={{ default: "nowrap" }}>
                        <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                          <FlexItem><Icon><CloneIcon /></Icon></FlexItem>
                          <FlexItem>Image Mirror</FlexItem>
                        </Flex>
                        <FlexItem>
                          <Label color="green" variant="outline" isCompact>Ready</Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    <DescriptionList isCompact>
                      <DescriptionListGroup>
                        <DescriptionListTerm>Source</DescriptionListTerm>
                        <DescriptionListDescription>
                          registry.redhat.io/rhoai
                        </DescriptionListDescription>
                      </DescriptionListGroup>
                      {status.imageMirror.name && (
                        <DescriptionListGroup>
                          <DescriptionListTerm>Name</DescriptionListTerm>
                          <DescriptionListDescription>
                            {status.imageMirror.name}
                          </DescriptionListDescription>
                        </DescriptionListGroup>
                      )}
                    </DescriptionList>
                  </CardBody>
                </Card>
              </GridItem>
            </Grid>
          </ExpandableSection>
        </StackItem>
      )}
    </Stack>
  );
};
