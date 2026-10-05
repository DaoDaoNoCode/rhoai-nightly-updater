import React from "react";
import { Flex, FlexItem } from "@patternfly/react-core";
import type { NightlyBuild } from "../types";
import { commitURL, formatBuildDate, shortCommit, shortDigest } from "../build";
import { formatRelativeTime } from "../utils";
import { CopyableText } from "./CopyableText";

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
  const built = formatBuildDate(build.buildDate);
  const commit = shortCommit(build.dashboardCommit);
  const href = commitURL(build.dashboardGitURL, build.dashboardCommit);
  const parts: React.ReactNode[] = [];
  parts.push(<strong key="tag">{build.tag || "custom image"}</strong>);
  if (digest) {
    parts.push(
      <span key="digest" className="rhoai-build-id">
        <CopyableText text={digest} value={build.image} what="image reference" />
      </span>,
    );
  }
  if (built && build.buildDate) {
    parts.push(
      <span key="built">
        built <time dateTime={build.buildDate} title={formatRelativeTime(build.buildDate)}>{built}</time>
      </span>,
    );
  }
  if (commit && !hideDashboard) {
    parts.push(
      <span key="dashboard">
        dashboard{" "}
        {href ? (
          <a className="rhoai-build-id" href={href} target="_blank" rel="noopener noreferrer" aria-label={`dashboard commit ${commit} on GitHub`}>{commit}</a>
        ) : (
          <span className="rhoai-build-id">{commit}</span>
        )}
      </span>,
    );
  }
  return (
    <Flex
      gap={{ default: "gapXs" }}
      alignItems={{ default: "alignItemsBaseline" }}
      flexWrap={{ default: "wrap" }}
      component="span"
      style={inline ? { display: "inline-flex", verticalAlign: "baseline" } : undefined}
    >
      {parts.map((part, i) => (
        <React.Fragment key={i}>
          {i > 0 && <FlexItem component="span" aria-hidden="true" className="rhoai-subtle">·</FlexItem>}
          <FlexItem component="span">{part}</FlexItem>
        </React.Fragment>
      ))}
    </Flex>
  );
};
