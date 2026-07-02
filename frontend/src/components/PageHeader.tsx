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
  lastRefreshed: Date | null;
  loading: boolean;
  onRefresh: () => void;
}

/**
 * Shared page header with title, "Last refreshed" timestamp, and a Refresh button.
 * Used by StatusPage, ComponentsPage, and DebugPage.
 */
export const PageHeader: React.FC<PageHeaderProps> = ({
  title,
  lastRefreshed,
  loading,
  onRefresh,
}) => (
  <PageSection padding={{ default: "padding" }}>
    <Flex
      justifyContent={{ default: "justifyContentSpaceBetween" }}
      alignItems={{ default: "alignItemsCenter" }}
    >
      <FlexItem>
        <Title headingLevel="h2">{title}</Title>
      </FlexItem>
      <FlexItem>
        <Flex
          alignItems={{ default: "alignItemsCenter" }}
          gap={{ default: "gapSm" }}
        >
          {lastRefreshed && (
            <FlexItem>
              <Content component="small">
                Last refreshed: {lastRefreshed.toLocaleString()}
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
