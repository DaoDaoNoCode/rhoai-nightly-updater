import React, { useState } from "react";
import { Card, CardBody, CardExpandableContent, CardHeader, CardTitle, Content, Flex, FlexItem, Spinner, Title, Tooltip } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { DashboardContainerBuild, DashboardDevImage } from "../types";
import type { DashboardBuildDetails } from "../hooks/useDashboardBuilds";
import { isBuildContainer } from "../hooks/useDashboardBuilds";
import { ImageRef } from "./ImageRef";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel, TagLabel, type StatusKind } from "./StatusLabel";
import { TruncatedText } from "./LongText";

export function componentName(container: string): string {
  if (container === "rhods-dashboard") return "Dashboard";
  if (container === "core-bff") return "Core BFF";
  const acronyms: Record<string, string> = { automl: "AutoML", autorag: "AutoRAG", maas: "MaaS", mlflow: "MLflow", ai: "AI" };
  return container.replace(/-ui$/, "").split("-").map(word => acronyms[word] || word[0]?.toUpperCase() + word.slice(1)).join(" ");
}

function imageTagOf(image: string): string {
  const reference = image.split("@", 2)[0];
  const separator = reference.lastIndexOf(":");
  return separator > reference.lastIndexOf("/") ? reference.slice(separator + 1) : "";
}

/**
 * The immutable tags Deploy pins main builds under: main-<7..40 hex> (the
 * OpenShift CI mirror of :main) and the full 40-hex commit (Konflux's push
 * builds, next to :odh-stable).
 */
const ODH_COMMIT_TAG = /^main-[0-9a-f]{7,40}$/;
const COMMIT_SHA_TAG = /^[0-9a-f]{40}$/;

/** A commit tag shown short: "21c06a3" for the 40-hex tag, "main-21c06a3" as is. */
function shortTag(tag: string): string {
  return COMMIT_SHA_TAG.test(tag) ? tag.slice(0, 7) : tag;
}

/** What a container runs: PR build, main build, the release image, or something else. */
export function runningLabel(image: DashboardDevImage): { text: string; color: "blue" | "purple" | "grey" | "orange" } {
  switch (image.running) {
    case "pr":
      return { text: image.runningPR ? `PR #${image.runningPR}` : "PR build", color: "blue" };
    case "main":
      return { text: "Latest main", color: "purple" };
    case "release":
      return { text: "Release", color: "grey" };
    case "other":
      return { text: "Other image", color: "orange" };
  }
  // Older backends: infer from the tag.
  const tag = imageTagOf(image.currentImage);
  if (/^(odh-)?pr-\d+$/.test(tag)) return { text: `PR #${tag.replace(/^(odh-)?pr-/, "")}`, color: "blue" };
  if (tag === "main" || tag === "odh-stable" || ODH_COMMIT_TAG.test(tag) || COMMIT_SHA_TAG.test(tag)) return { text: "Latest main", color: "purple" };
  if (image.currentImage === image.defaultImage) return { text: "Release", color: "grey" };
  return { text: "Other image", color: "orange" };
}

/** Status of one container, with the pod's own reason when it is not ready (A04-7). */
function statusOf(image: DashboardDevImage): { text: string; detail?: string; kind: "ok" | "progress" | "problem" } {
  if (image.rolloutStuck) return { text: "Rollout stuck", detail: image.rolloutMessage || image.waitingMessage, kind: "problem" };
  if (image.waitingReason) {
    const restarts = image.restarts ? ` (${image.restarts} restarts)` : "";
    return { text: `${image.waitingReason}${restarts}`, detail: image.waitingMessage, kind: image.waitingReason === "ContainerCreating" || image.waitingReason === "PodInitializing" ? "progress" : "problem" };
  }
  if (image.matchesTarget === false) return { text: "Not updated", detail: image.targetTag ? `Target: ${image.targetTag}` : undefined, kind: "problem" };
  if (!image.ready) return { text: "Rolling out", kind: "progress" };
  return { text: "Ready", kind: "ok" };
}

export function imageProblems(images: DashboardDevImage[]): number {
  return images.filter((i) => statusOf(i).kind === "problem").length;
}

const RUNNING_COLORS: Record<ReturnType<typeof runningLabel>["color"], "blue" | "purple" | "grey" | "teal"> = {
  blue: "blue", purple: "purple", grey: "grey", orange: "teal",
};

function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

/** "15 min" or "40 s" for a retry delay. */
export function retryIn(seconds: number): string {
  return seconds >= 60 ? `${Math.ceil(seconds / 60)} min` : `${seconds} s`;
}

/**
 * The first 7 hex of a commit as a link to GitHub, the full SHA in a tooltip
 * (and for screen readers, as RelativeTime does for its absolute date).
 */
const CommitLink: React.FC<{ commit: string; url?: string }> = ({ commit, url }) => {
  const short = <code>{commit.slice(0, 7)}</code>;
  return (
    <>
      <Tooltip content={commit}>
        {url ? <a href={url} target="_blank" rel="noopener noreferrer">{short}</a> : <span>{short}</span>}
      </Tooltip>
      <span className="pf-v6-screen-reader"> ({commit})</span>
    </>
  );
};

/** The chip for a build's distance from the branch it tracks. */
export function divergenceLabel(b: DashboardContainerBuild, retryAfterSeconds?: number): { status: StatusKind; text: string; tooltip?: string; link?: string } | null {
  const d = b.divergence;
  const pr = b.running === "pr" ? `PR #${b.pr}` : "";
  const base = pr || "main";
  if (!d) {
    if (b.commit || !b.reason) return null;
    return { status: "neutral", text: "Commit unknown", tooltip: b.message || b.reason };
  }
  switch (d.status) {
    case "identical":
      return { status: "success", text: pr ? `Latest commit of ${pr}` : "Up to date with main" };
    case "behind":
      // Informational: a build is normally a few commits behind a moving branch.
      return { status: "info", text: pr ? `${pr} has ${plural(d.behind, "newer commit")}` : `${plural(d.behind, "commit")} behind main`, link: d.compareURL };
    case "ahead":
    case "diverged":
      return { status: "warning", text: `Not on ${base}`, link: d.compareURL, tooltip: "An unmerged or rebased commit: the build has commits the branch lacks." };
    default: {
      const tooltip = d.reason === "rate_limited"
        ? `GitHub rate limit; retry in ${retryIn(retryAfterSeconds ?? 60)}`
        : d.message || d.reason || "GitHub could not be asked.";
      return { status: "neutral", text: `Distance from ${base} unknown`, tooltip };
    }
  }
}

const DivergenceChip: React.FC<{ build: DashboardContainerBuild; retryAfterSeconds?: number }> = ({ build, retryAfterSeconds }) => {
  const label = divergenceLabel(build, retryAfterSeconds);
  if (!label) return null;
  const chip = <StatusLabel status={label.status}>{label.text}</StatusLabel>;
  return (
    <>
      {/* One item, so the Compare link wraps with its chip in a narrow column. */}
      <FlexItem className="pf-v6-u-text-nowrap">
        {label.tooltip ? (
          <>
            <Tooltip content={label.tooltip}><span>{chip}</span></Tooltip>
            <span className="pf-v6-screen-reader"> ({label.tooltip})</span>
          </>
        ) : chip}
        {label.link && (
          <a className="pf-v6-u-ml-sm pf-v6-u-font-size-sm" href={label.link} target="_blank" rel="noopener noreferrer">Compare <ExternalLinkAltIcon /></a>
        )}
      </FlexItem>
      {build.divergence?.prState === "merged" && <FlexItem><TagLabel>PR #{build.pr} merged</TagLabel></FlexItem>}
    </>
  );
};

interface DashboardImagesProps {
  images: DashboardDevImage[];
  defaultExpanded?: boolean;
  /** Build commits from GET /api/dashboard/builds; absent on pages that do not load them. */
  buildDetails?: DashboardBuildDetails;
}

/** Per-container images of the dashboard as an expandable card: what runs where, and why a container is not ready. */
export const DashboardImages: React.FC<DashboardImagesProps> = ({ images, defaultExpanded = false, buildDetails }) => {
  const problems = imageProblems(images);
  const [expanded, setExpanded] = useState(defaultExpanded || problems > 0);
  const ready = images.filter((image) => statusOf(image).kind === "ok").length;
  const counts = new Map<string, number>();
  for (const image of images) {
    const t = runningLabel(image).text;
    counts.set(t, (counts.get(t) ?? 0) + 1);
  }
  const summary = [...counts.entries()].map(([t, n]) => `${n} ${t}`).join(", ");
  // Details describe one image: after a deploy switched a container, the
  // previous build's commit must not sit next to the new reference while the
  // next fetch runs (or fails).
  const buildFor = (image: DashboardDevImage) => buildDetails?.builds.find((b) => b.deployment === image.deployment && b.container === image.container && b.image === image.currentImage);

  return (
    <Card isExpanded={expanded}>
      <CardHeader
        onExpand={() => setExpanded(!expanded)}
        toggleButtonProps={{ id: "dashboard-containers-toggle", "aria-label": "Dashboard containers details", "aria-expanded": expanded, "aria-labelledby": "dashboard-containers-toggle dashboard-containers-title" }}
        actions={{
          actions: (
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
              <FlexItem>
                <StatusLabel status={ready === images.length ? "success" : problems > 0 ? "danger" : "progress"}>{ready}/{images.length} ready</StatusLabel>
              </FlexItem>
              {problems > 0 && <FlexItem><StatusLabel status="danger">{problems} with problems</StatusLabel></FlexItem>}
            </Flex>
          ),
          hasNoOffset: true,
        }}
      >
        <CardTitle><Title headingLevel="h2" size="lg" id="dashboard-containers-title">Dashboard containers ({images.length})</Title></CardTitle>
      </CardHeader>
      <CardExpandableContent>
        <CardBody>
          <Content component="p" className="pf-v6-u-text-color-subtle">Running: {summary}.</Content>
          {buildDetails?.rateLimited && (
            <Content component="p" className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">
              GitHub&apos;s rate limit is reached: distances from main are unknown for {retryIn(buildDetails.retryAfterSeconds ?? 60)}. A <code>GITHUB_TOKEN</code> on the updater raises the limit.
            </Content>
          )}
          {buildDetails?.error && (
            <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">
              <TruncatedText>Build details could not be loaded: {buildDetails.error.message}</TruncatedText>
            </div>
          )}
        </CardBody>
        <CardBody className="pf-v6-u-px-0">
              <Table aria-label="Dashboard container images" variant="compact" borders={false}>
                <Thead><Tr><Th width={25}>Component</Th><Th width={45}>Running</Th><Th width={30}>Status</Th></Tr></Thead>
                <Tbody>
                  {images.map((image, rowIndex) => {
                    const running = runningLabel(image);
                    const status = statusOf(image);
                    const tag = imageTagOf(image.currentImage);
                    const build = buildFor(image);
                    const checking = !build && !!buildDetails?.loading && isBuildContainer(image.running);
                    return (
                      <Tr key={`${image.deployment}/${image.container}`} isStriped={rowIndex % 2 === 1}>
                        <Td dataLabel="Component">
                          <strong>{componentName(image.container)}</strong>
                          <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{image.deployment}</div>
                        </Td>
                        <Td dataLabel="Running">
                          <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} className="pf-v6-u-mb-xs">
                            <FlexItem><TagLabel color={RUNNING_COLORS[running.color]}>{running.text}</TagLabel></FlexItem>
                            {build?.commit
                              ? <FlexItem><CommitLink commit={build.commit} url={build.commitURL} /></FlexItem>
                              : tag && running.text !== "Release" && <FlexItem><code>{shortTag(tag)}</code></FlexItem>}
                            {image.flavor && running.text !== "Release" && <FlexItem className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{image.flavor === "odh" ? "ODH build" : "RHOAI build"}</FlexItem>}
                            {build && <DivergenceChip build={build} retryAfterSeconds={buildDetails?.retryAfterSeconds} />}
                            {checking && (
                              <FlexItem className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">
                                <Spinner size="sm" aria-hidden="true" className="pf-v6-u-mr-xs" />Checking...
                              </FlexItem>
                            )}
                          </Flex>
                          {build && (build.buildDate || build.commitDate) && (
                            <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle pf-v6-u-mb-xs">
                              {build.buildDate && <RelativeTime date={build.buildDate} prefix="built " size="inherit" />}
                              {build.buildDate && build.commitDate && " · "}
                              {build.commitDate && <RelativeTime date={build.commitDate} prefix="committed " size="inherit" />}
                            </div>
                          )}
                          {build?.newer && (
                            <div className="pf-v6-u-font-size-sm pf-v6-u-mb-xs">
                              Newer build available: {build.newer.commit ? <CommitLink commit={build.newer.commit} url={build.newer.commitURL} /> : <code>{build.newer.tag}</code>}
                              {build.newer.buildDate && <> <RelativeTime date={build.newer.buildDate} prefix="built " size="inherit" /></>}
                            </div>
                          )}
                          <ImageRef image={image.currentImage} />
                        </Td>
                        <Td dataLabel="Status">
                          <StatusLabel status={status.kind === "ok" ? "success" : status.kind === "progress" ? "progress" : "danger"}>
                            {status.text}
                          </StatusLabel>
                          {status.detail && <div className="pf-v6-u-font-size-sm pf-v6-u-text-break-word"><TruncatedText>{status.detail}</TruncatedText></div>}
                          {image.podName && status.kind !== "ok" && <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">Pod: {image.podName}</div>}
                        </Td>
                      </Tr>
                    );
                  })}
                </Tbody>
              </Table>
        </CardBody>
      </CardExpandableContent>
    </Card>
  );
};
