import React from "react";
import {
  Button,
  Content,
  Flex,
  FlexItem,
  PageSection,
  Title,
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";

interface PageHeaderProps {
  title: string;
  /** One sentence on what the page is for. */
  description?: React.ReactNode;
  lastRefreshed: Date | null;
  loading: boolean;
  onRefresh: () => void;
}

/**
 * Shared page header: title, optional description, "Last refreshed" time and
 * a Refresh button.
 */
export const PageHeader: React.FC<PageHeaderProps> = ({
  title,
  description,
  lastRefreshed,
  loading,
  onRefresh,
}) => (
  <PageSection padding={{ default: "padding" }}>
    <Flex
      justifyContent={{ default: "justifyContentSpaceBetween" }}
      alignItems={{ default: "alignItemsFlexStart" }}
      gap={{ default: "gapSm" }}
    >
      <FlexItem flex={{ default: "flex_1" }} style={{ minWidth: "16rem" }}>
        <Title headingLevel="h1" size="xl">{title}</Title>
        {description && <Content component="p" className="rhoai-subtle" style={{ marginTop: "var(--pf-t--global--spacer--xs)" }}>{description}</Content>}
      </FlexItem>
      <FlexItem>
        <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }}>
          {lastRefreshed && (
            <FlexItem>
              <Content component="small">
                Updated <time dateTime={lastRefreshed.toISOString()}>{lastRefreshed.toLocaleTimeString()}</time>
              </Content>
            </FlexItem>
          )}
          <FlexItem>
            <Button
              variant="link"
              onClick={onRefresh}
              isDisabled={loading}
              icon={<SyncAltIcon />}
              aria-label={`Refresh ${title.toLowerCase()}`}
            >
              Refresh
            </Button>
          </FlexItem>
        </Flex>
      </FlexItem>
    </Flex>
  </PageSection>
);
