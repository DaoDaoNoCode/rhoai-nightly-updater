import React from "react";
import { Alert, AlertActionLink, Content, Spinner, Stack, StackItem } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td, type IAction } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { PRContainsBuild } from "../types";
import type { PRSearchState } from "../hooks/usePRSearch";
import type { BuildSide } from "./BuildCompare";
import { NotRecorded, RelativeTime } from "./RelativeTime";
import { RowActions } from "./RowActions";
import { StatusLabel } from "./StatusLabel";

const REPO = "opendatahub-io/odh-dashboard";

const UNKNOWN_REASONS: Record<string, string> = {
  rate_limited: "GitHub rate limit",
  commit_not_found: "commit not on GitHub",
  no_component_image: "no dashboard image in this build",
  no_commit_label: "no commit label",
  unexpected_repo: "built from another repository",
  build_unreadable: "build not readable",
  github_error: "GitHub error",
};

function sentence(text: string): string {
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

function minutes(seconds: number): string {
  const m = Math.max(1, Math.ceil(seconds / 60));
  return m === 1 ? "about a minute" : `about ${m} minutes`;
}

const ResultLabel: React.FC<{ answer: PRContainsBuild; pr: string }> = ({ answer, pr }) => {
  if (answer.result === "contains") return <StatusLabel status="success">Contains PR #{pr}</StatusLabel>;
  if (answer.result === "not_contained") return <StatusLabel status="neutral">Does not contain</StatusLabel>;
  const reason = UNKNOWN_REASONS[answer.reason ?? ""] ?? "unknown";
  return <StatusLabel status="warning">Unknown ({reason})</StatusLabel>;
};

const EXPLANATION = (
  <>
    A build contains the PR when its odh-dashboard commit ({REPO} PRs are merged into red-hat-data-services/odh-dashboard
    release branches) includes the PR&apos;s merge commit. A change that reached a release branch only as a cherry-pick has a
    different commit and shows as &quot;does not contain&quot;. The search covers the installed build and the builds the filters list.
  </>
);

/** Results of "which builds contain PR #N" in the Build Explorer search. */
export const PRSearchResults: React.FC<{
  pr: string;
  search: PRSearchState;
  /** Builds about to be searched (before the first progress update). */
  plannedTotal: number;
  canCompare: boolean;
  /** The installed build gets no "Compare with installed", whatever its label. */
  isInstalledImage: (image: string) => boolean;
  onCompare: (to: BuildSide) => void;
  onRetry: () => void;
}> = ({ pr, search, plannedTotal, canCompare, isInstalledImage, onCompare, onRetry }) => {
  const prURL = search.pr?.url || `https://github.com/${REPO}/pull/${pr}`;
  const prLink = (
    <a href={prURL} target="_blank" rel="noopener noreferrer">PR #{pr} <ExternalLinkAltIcon /></a>
  );

  if (search.error) {
    const code = search.error.errorCode;
    const title = code === "pr_not_merged" ? `PR #${pr} is not merged`
      : code === "pr_not_found" ? `${REPO} has no PR #${pr}`
      : `Could not search for PR #${pr}`;
    return (
      <Alert component="p" variant={code === "pr_not_merged" || code === "pr_not_found" ? "warning" : "danger"} isInline isLiveRegion title={title}
        actionLinks={code === "pr_not_merged" || code === "pr_not_found" ? undefined : <AlertActionLink onClick={onRetry}>Retry</AlertActionLink>}>
        {sentence(search.error.message)} {code === "pr_not_merged" ? <>Only merged PRs reach a build. {prLink}</> : null}
      </Alert>
    );
  }

  if (plannedTotal === 0) {
    return <Content component="p">There are no builds to search yet. Wait for the build list to load, or change the filters.</Content>;
  }

  const total = search.total || plannedTotal;
  const running = search.running || search.total === 0;
  const contains = search.results.filter((r) => r.answer.result === "contains").length;
  const unknown = search.results.filter((r) => r.answer.result === "unknown").length;
  const mergeCommit = search.pr?.mergeCommit;

  return (
    <Stack hasGutter>
      {search.pr && (
        <StackItem>
          <Content component="p">
            {prLink}{search.pr.title ? <>: {search.pr.title}</> : null}
            {search.pr.mergedAt && <>. Merged <RelativeTime date={search.pr.mergedAt} size="inherit" /></>}
            {mergeCommit && <> as <code>{mergeCommit.slice(0, 12)}</code></>}.
          </Content>
        </StackItem>
      )}
      <StackItem>
        <Content component="p" aria-live="polite">
          {running
            ? <><Spinner size="sm" aria-label="Searching builds" />{" "}Checked {search.checked} of {total} builds for PR #{pr}...</>
            : <>Checked {total} builds: {contains === 0 ? "none contains" : `${contains} ${contains === 1 ? "contains" : "contain"}`} PR #{pr}{unknown > 0 ? `, ${unknown} unknown` : ""}.</>}
        </Content>
      </StackItem>
      {search.rateLimited && !running && (
        <StackItem>
          <Alert component="p" variant="warning" isInline isPlain title="GitHub's rate limit was reached"
            actionLinks={<AlertActionLink onClick={onRetry}>Search again</AlertActionLink>}>
            Builds marked unknown could not be checked. Try again in {minutes(search.retryAfterSeconds)}. Without a GITHUB_TOKEN on the
            server, GitHub allows 60 requests per hour for the whole cluster.
          </Alert>
        </StackItem>
      )}
      {search.results.length > 0 && (
        <StackItem>
          <Table aria-label={`Builds and PR #${pr}`} variant="compact" borders={false}>
            <Thead>
              <Tr>
                <Th width={25}>Build</Th>
                <Th info={{ popover: <Content component="p">{EXPLANATION}</Content>, popoverProps: { headerContent: `How "contains PR #${pr}" is decided` } }}>
                  PR #{pr}
                </Th>
                <Th>Dashboard commit</Th>
                <Th screenReaderText="Actions" />
              </Tr>
            </Thead>
            <Tbody>
              {search.results.map(({ build, answer }, rowIndex) => {
                const actions: IAction[] = [];
                if (answer.compareURL) {
                  actions.push({
                    title: "GitHub comparison",
                    to: answer.compareURL,
                    isExternalLink: true,
                    "aria-label": `GitHub comparison of PR #${pr}'s merge commit with ${build.label}`,
                  } as IAction);
                }
                if (canCompare && !isInstalledImage(build.image)) {
                  actions.push({ title: "Compare with installed", onClick: () => onCompare({ image: build.image, label: build.label }) });
                }
                return (
                  <Tr key={build.image} isStriped={rowIndex % 2 === 1}>
                    <Td dataLabel="Build"><strong>{build.label}</strong></Td>
                    <Td dataLabel={`PR #${pr}`}>
                      <ResultLabel answer={answer} pr={pr} />
                      {answer.result === "unknown" && answer.message && (
                        <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{answer.message}</div>
                      )}
                    </Td>
                    <Td dataLabel="Dashboard commit">
                      {answer.commit && answer.commitRepo
                        ? <a href={`https://github.com/${answer.commitRepo}/commit/${answer.commit}`} target="_blank" rel="noopener noreferrer" title={`${answer.commitRepo}@${answer.commit}`}><code>{answer.commit.slice(0, 7)}</code></a>
                        : <NotRecorded what="No dashboard commit recorded" />}
                    </Td>
                    <Td isActionCell>{actions.length > 0 && <RowActions items={actions} rowName={build.label} />}</Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        </StackItem>
      )}
    </Stack>
  );
};
