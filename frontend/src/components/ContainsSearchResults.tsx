import React from "react";
import { Alert, AlertActionLink, Content, Spinner, Stack, StackItem } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td, type IAction } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { PRContainsBuild } from "../types";
import type { ContainsQuery } from "../services/api";
import { cutoffOf, type ContainsSearchResult, type ContainsSearchState } from "../hooks/useContainsSearch";
import type { SearchBuild } from "../hooks/useCommitSearch";
import type { BuildSide } from "./BuildCompare";
import { imageTag, shortBuildRef } from "./buildDiff";
import { NotRecorded, RelativeTime } from "./RelativeTime";
import { RowActions } from "./RowActions";
import { StatusLabel } from "./StatusLabel";

export const DEFAULT_REPO = "opendatahub-io/odh-dashboard";
const FORK_OWNER = "red-hat-data-services";

/** The repository name without its owner: "odh-dashboard". */
function repoName(repo: string): string {
  return repo.split("/")[1] || repo;
}

function unknownReasons(kind: ContainsQuery["kind"], name: string): Record<string, string> {
  return {
    rate_limited: "GitHub rate limit",
    commit_not_found: kind === "pr" ? "merge commit not in the fork" : "commit not in the fork",
    no_component_image: `no ${name} image in this build`,
    no_commit_label: "no commit label",
    unexpected_repo: "built from another repository",
    build_unreadable: "build not readable",
    github_error: "GitHub error",
  };
}

/** The subtle text under "Does not contain" for a build answered from its date alone. */
const CUTOFF_TEXT: Record<string, string> = {
  built_before_merge: "Built before the merge",
  built_before_commit: "Built before the commit",
};

function sentence(text: string): string {
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

function minutes(seconds: number): string {
  const m = Math.max(1, Math.ceil(seconds / 60));
  return m === 1 ? "about a minute" : `about ${m} minutes`;
}

function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

/** "a", "a and b", "a, b and c", "a, b, c and d", or "a, b and 3 more builds". */
export function listNames(names: string[]): string {
  if (names.length <= 4) {
    if (names.length <= 1) return names[0] ?? "";
    return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
  }
  return `${names[0]}, ${names[1]} and ${names.length - 2} more builds`;
}

/** How a build is named in a sentence: its tag; the installed one as "rhoai-3.6 (installed)". */
export function sentenceName(build: SearchBuild): string {
  if (build.label.startsWith("Installed (")) return `${imageTag(build.image) || shortBuildRef(build.image)} (installed)`;
  return build.label;
}

/** The subtle line after the answer: the builds that do not contain it, and those that could not be checked. */
export function detailLine(kind: ContainsQuery["kind"], results: ContainsSearchResult[], name: string): string {
  const before = kind === "pr" ? "built before the merge" : "built before the commit";
  const named = results.filter((r) => r.answer.result === "not_contained" && !r.answer.reason).map((r) => sentenceName(r.build));
  const older = results.filter((r) => r.answer.result === "not_contained" && !!r.answer.reason).length;
  const unknown = results.filter((r) => r.answer.result === "unknown");
  const parts: string[] = [];
  if (named.length > 0 && older > 0) parts.push(`Not in ${listNames(named)} and ${plural(older, "older build")} (${before}).`);
  else if (named.length > 0) parts.push(`Not in ${listNames(named)}.`);
  else if (older > 0) parts.push(`Not in ${plural(older, "build")} ${before}.`);
  if (unknown.length > 0) {
    const reasons = unknownReasons(kind, name);
    const why = [...new Set(unknown.map((r) => reasons[r.answer.reason ?? ""] ?? "unknown"))].join(", ");
    parts.push(`${plural(unknown.length, "build")} could not be checked (${why}).`);
  }
  return parts.join(" ");
}

const ResultLabel: React.FC<{ answer: PRContainsBuild; subject: string; reasons: Record<string, string> }> = ({ answer, subject, reasons }) => {
  if (answer.result === "contains") return <StatusLabel status="success">Contains {subject}</StatusLabel>;
  if (answer.result === "not_contained") return <StatusLabel status="neutral">Does not contain</StatusLabel>;
  const reason = reasons[answer.reason ?? ""] ?? "unknown";
  return <StatusLabel status="warning">Unknown ({reason})</StatusLabel>;
};

/** Results of "which builds contain PR #N" or "which builds contain commit X" in the Build Explorer search. */
export const ContainsSearchResults: React.FC<{
  query: ContainsQuery;
  search: ContainsSearchState;
  /** Builds about to be searched (before the first progress update). */
  plannedTotal: number;
  canCompare: boolean;
  /** The installed build gets no "Compare with installed", whatever its label. */
  isInstalledImage: (image: string) => boolean;
  onCompare: (to: BuildSide) => void;
  onRetry: () => void;
}> = ({ query, search, plannedTotal, canCompare, isInstalledImage, onCompare, onRetry }) => {
  const { kind } = query;
  const repo = query.repo || DEFAULT_REPO;
  const name = repoName(repo);
  const fork = repo.toLowerCase().startsWith(`${FORK_OWNER}/`) ? repo : `${FORK_OWNER}/${name}`;
  const info = search.info;
  const reasons = unknownReasons(kind, name);
  // "PR #5123" or "Commit 6c8ac6e"; the sentence adds the commit's title.
  const sha = (kind === "commit" && (info?.commit || query.value)) || "";
  const subject = kind === "pr" ? `PR #${query.value}` : `Commit ${sha.slice(0, 7)}`;
  const subjectWithTitle = kind === "commit" && info?.commitTitle ? `${subject} (${info.commitTitle})` : subject;
  const subjectURL = kind === "pr" ? info?.url || `https://github.com/${repo}/pull/${query.value}` : info?.commitURL || "";
  const subjectLink = subjectURL
    ? <a href={subjectURL} target="_blank" rel="noopener noreferrer">{subject} <ExternalLinkAltIcon /></a>
    : <>{subject}</>;

  if (search.error) {
    const code = search.error.errorCode;
    const expected = code === "pr_not_merged" || code === "pr_not_found" || code === "commit_not_found";
    const title = code === "pr_not_merged" ? `${subject} is not merged`
      : code === "pr_not_found" ? `${repo} has no ${subject}`
      : code === "commit_not_found" ? `GitHub does not know ${subject.toLowerCase()}`
      : `Could not search for ${subject}`;
    return (
      <Alert component="p" variant={expected ? "warning" : "danger"} isInline isLiveRegion title={title}
        actionLinks={expected ? undefined : <AlertActionLink onClick={onRetry}>Retry</AlertActionLink>}>
        {sentence(search.error.message)} {code === "pr_not_merged" ? <>Only merged PRs reach a build. {subjectLink}</> : null}
      </Alert>
    );
  }

  if (plannedTotal === 0) {
    return <Content component="p">There are no builds to search yet. Wait for the build list to load, or change the filters.</Content>;
  }

  const total = search.total || plannedTotal;
  const running = search.running || search.total === 0;
  const containing = search.results.filter((r) => r.answer.result === "contains").map((r) => sentenceName(r.build));
  const cutoff = cutoffOf(info);
  const newestBuilt = search.results.reduce<number | null>((newest, r) => {
    const t = r.build.buildDate ? Date.parse(r.build.buildDate) : NaN;
    return Number.isNaN(t) ? newest : newest === null ? t : Math.max(newest, t);
  }, null);
  const detail = detailLine(kind, search.results, name);

  const explanation = (
    <>
      A build contains {kind === "pr" ? "the PR" : "the commit"} when the commit its {name} image was built from, in {fork}, includes
      {kind === "pr" ? " the PR's merge commit" : " it"} (GitHub compare). The fork merges {repo}, so upstream commits keep their SHA; a fork
      that cherry-picks or rebases instead does not have {kind === "pr" ? "the merge commit" : "the commit"}, and its builds show as unknown.
      Builds older than {kind === "pr" ? "the merge" : "the commit"} are not read. The search covers the installed build and the builds the filters list.
    </>
  );

  return (
    <Stack hasGutter>
      {info && (kind === "pr" ? (info.title || info.mergedAt) : (info.commitDate || info.commitURL)) && (
        <StackItem>
          <Content component="p">
            {subjectLink}
            {kind === "pr" ? (
              <>
                {info.title ? <>: {info.title}</> : null}
                {info.mergedAt && <>. Merged <RelativeTime date={info.mergedAt} size="inherit" /></>}
                {info.mergeCommit && <> as <code>{info.mergeCommit.slice(0, 12)}</code></>}.
              </>
            ) : (
              <>
                {" "}in {info.commitURL?.includes(`/${FORK_OWNER}/`) ? fork : repo}
                {info.commitDate && <>, committed <RelativeTime date={info.commitDate} size="inherit" /></>}.
              </>
            )}
          </Content>
        </StackItem>
      )}
      <StackItem>
        {running ? (
          <Content component="p" aria-live="polite">
            <Spinner size="sm" aria-label="Searching builds" />{" "}Checked {search.checked} of {total} builds for {subject}...
          </Content>
        ) : (
          <>
            <Content component="p" aria-live="polite">
              {containing.length > 0 ? <>{subjectWithTitle} is in {listNames(containing)}.</>
                : cutoff !== null && newestBuilt !== null && newestBuilt < cutoff ? (
                  <>
                    No build contains {subjectWithTitle} yet: it was {kind === "pr" ? "merged" : "committed"}{" "}
                    <RelativeTime date={new Date(cutoff)} size="inherit" />, after the newest build (<RelativeTime date={new Date(newestBuilt)} size="inherit" />).
                  </>
                )
                : <>No build contains {subjectWithTitle}.</>}
            </Content>
            {detail && <Content component="p" className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm">{detail}</Content>}
          </>
        )}
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
          <Table aria-label={`Builds and ${subject}`} variant="compact" borders={false}>
            <Thead>
              <Tr>
                <Th width={25}>Build</Th>
                <Th info={{ popover: <Content component="p">{explanation}</Content>, popoverProps: { headerContent: `How "contains ${subject}" is decided` } }}>
                  {subject}
                </Th>
                <Th>{name === "odh-dashboard" ? "Dashboard commit" : `${name} commit`}</Th>
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
                    "aria-label": `GitHub comparison of ${kind === "pr" ? `${subject}'s merge commit` : subject.toLowerCase()} with ${build.label}`,
                  } as IAction);
                }
                if (canCompare && !isInstalledImage(build.image)) {
                  actions.push({ title: "Compare with installed", onClick: () => onCompare({ image: build.image, label: build.label, buildDate: build.buildDate }) });
                }
                const note = answer.result === "unknown" ? answer.message : answer.result === "not_contained" ? CUTOFF_TEXT[answer.reason ?? ""] : undefined;
                return (
                  <Tr key={build.image} isStriped={rowIndex % 2 === 1}>
                    <Td dataLabel="Build"><strong>{build.label}</strong></Td>
                    <Td dataLabel={subject}>
                      <ResultLabel answer={answer} subject={subject} reasons={reasons} />
                      {note && <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{note}</div>}
                    </Td>
                    <Td dataLabel="Commit">
                      {answer.commit && answer.commitRepo
                        ? <a href={`https://github.com/${answer.commitRepo}/commit/${answer.commit}`} target="_blank" rel="noopener noreferrer" title={`${answer.commitRepo}@${answer.commit}`}><code>{answer.commit.slice(0, 7)}</code></a>
                        : <NotRecorded what={answer.reason && CUTOFF_TEXT[answer.reason] ? `Not read: ${CUTOFF_TEXT[answer.reason].toLowerCase()}` : `No ${name} commit recorded`} />}
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
