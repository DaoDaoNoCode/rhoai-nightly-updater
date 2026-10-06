import React from "react";
import {
  ActionList,
  ActionListGroup,
  ActionListItem,
  Button,
  Content,
  Flex,
  FlexItem,
  PageSection,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import SyncAltIcon from "@patternfly/react-icons/dist/esm/icons/sync-alt-icon";
import { GlobalBanners } from "./GlobalBanners";
import { RelativeTime } from "./RelativeTime";

interface PageHeaderProps {
  title: string;
  /** One sentence on what the page is for. */
  description: React.ReactNode;
  /** When the data on the page was last loaded. */
  lastRefreshed?: Date | null;
  loading?: boolean;
  /** Reload the page's data. Omit for pages without a refresh. */
  onRefresh?: () => void;
  /** Text of the refresh button (default "Refresh"). */
  refreshText?: string;
  /** Extra page actions, shown before the refresh control. */
  actions?: React.ReactNode;
  /** Key facts under the description (for example the installed build). */
  details?: React.ReactNode;
}

/**
 * The header of every page: H1, a one-sentence description, the page
 * actions with the refresh control and its "Updated" time on the right, and
 * the app-wide notices right under the title (PatternFly Alert guidance:
 * page-level alerts go in the page header, below the title).
 */
export const PageHeader: React.FC<PageHeaderProps> = ({
  title,
  description,
  lastRefreshed,
  loading = false,
  onRefresh,
  refreshText = "Refresh",
  actions,
  details,
}) => (
  <PageSection>
    <Stack hasGutter>
      <StackItem>
        <Flex
          direction={{ default: "column", md: "row" }}
          justifyContent={{ md: "justifyContentSpaceBetween" }}
          alignItems={{ default: "alignItemsFlexStart" }}
          gap={{ default: "gapSm", md: "gapMd" }}
          flexWrap={{ default: "nowrap" }}
        >
          <FlexItem flex={{ default: "flex_1" }}>
            <Flex direction={{ default: "column" }} gap={{ default: "gapXs" }}>
              <FlexItem>
                <Title headingLevel="h1">{title}</Title>
              </FlexItem>
              <FlexItem>
                <Content component="p" className="pf-v6-u-text-color-subtle">{description}</Content>
              </FlexItem>
            </Flex>
          </FlexItem>
          {(actions || onRefresh) && (
            <FlexItem>
              <ActionList>
                {actions && <ActionListGroup>{actions}</ActionListGroup>}
                {onRefresh && (
                  <ActionListItem>
                    <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
                      {lastRefreshed && <RelativeTime date={lastRefreshed} prefix="Updated " />}
                      <Button
                        variant="link"
                        icon={<SyncAltIcon />}
                        isLoading={loading}
                        onClick={() => { if (!loading) onRefresh(); }}
                        aria-label={`${refreshText} ${title.toLowerCase()}`}
                      >
                        {refreshText}
                      </Button>
                    </Flex>
                  </ActionListItem>
                )}
              </ActionList>
            </FlexItem>
          )}
        </Flex>
      </StackItem>
      {details && <StackItem>{details}</StackItem>}
      <GlobalBanners />
    </Stack>
  </PageSection>
);
