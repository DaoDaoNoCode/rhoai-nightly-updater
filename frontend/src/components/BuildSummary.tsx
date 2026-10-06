import React from "react";
import { Flex, FlexItem } from "@patternfly/react-core";
import type { NightlyBuild } from "../types";
import { commitURL, shortCommit, shortDigest } from "../build";
import { ImageRef } from "./ImageRef";
import { RelativeTime } from "./RelativeTime";

interface BuildSummaryProps {
  build: NightlyBuild;
  /** Hide the dashboard commit (e.g. while it is still being looked up). */
  hideDashboard?: boolean;
  /** Flow inside a sentence instead of taking its own line. */
  inline?: boolean;
}

/**
 * One nightly build on one line: tag · short digest · build date ·
 * dashboard commit. The digest copies the full image reference; the commit
 * links to GitHub when the backend gave a GitHub URL.
 */
export const BuildSummary: React.FC<BuildSummaryProps> = ({ build, hideDashboard, inline }) => {
  const digest = shortDigest(build.digest);
  const commit = shortCommit(build.dashboardCommit);
  const href = commitURL(build.dashboardGitURL, build.dashboardCommit);
  const parts: React.ReactNode[] = [];
  parts.push(<strong key="tag">{build.tag || "custom image"}</strong>);
  if (digest) {
    parts.push(<ImageRef key="digest" image={build.image} display={digest} />);
  }
  if (build.buildDate) {
    parts.push(<RelativeTime key="built" date={build.buildDate} prefix="built " size="inherit" />);
  }
  if (commit && !hideDashboard) {
    parts.push(
      <span key="dashboard">
        dashboard{" "}
        {href ? (
          <a className="pf-v6-u-font-family-monospace" href={href} target="_blank" rel="noopener noreferrer" aria-label={`dashboard commit ${commit} on GitHub`}>{commit}</a>
        ) : (
          <code>{commit}</code>
        )}
      </span>,
    );
  }
  return (
    <Flex
      gap={{ default: "gapSm" }}
      alignItems={{ default: "alignItemsCenter" }}
      flexWrap={{ default: "wrap" }}
      component="span"
      display={inline ? { default: "inlineFlex" } : undefined}
    >
      {parts.map((part, i) => (
        <React.Fragment key={i}>
          {i > 0 && <FlexItem component="span" aria-hidden="true" className="pf-v6-u-text-color-subtle">·</FlexItem>}
          <FlexItem component="span">{part}</FlexItem>
        </React.Fragment>
      ))}
    </Flex>
  );
};
