import React, { useState } from "react";
import { Content, ExpandableSection, Flex, FlexItem, Label } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import CheckCircleIcon from "@patternfly/react-icons/dist/esm/icons/check-circle-icon";
import ExclamationCircleIcon from "@patternfly/react-icons/dist/esm/icons/exclamation-circle-icon";
import InProgressIcon from "@patternfly/react-icons/dist/esm/icons/in-progress-icon";
import type { DashboardDevImage } from "../types";
import { ImageRef } from "./ImageRef";

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
  if (tag === "main" || tag === "odh-stable") return { text: "Latest main", color: "purple" };
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

/** Per-container images of the dashboard: what runs where, and why a container is not ready. */
export const DashboardImages: React.FC<{ images: DashboardDevImage[]; defaultExpanded?: boolean }> = ({ images, defaultExpanded = false }) => {
  const problems = imageProblems(images);
  const [expanded, setExpanded] = useState(defaultExpanded || problems > 0);
  const ready = images.filter((image) => statusOf(image).kind === "ok").length;
  const counts = new Map<string, number>();
  for (const image of images) {
    const t = runningLabel(image).text;
    counts.set(t, (counts.get(t) ?? 0) + 1);
  }
  const summary = [...counts.entries()].map(([t, n]) => `${n} ${t}`).join(", ");

  return (
    <ExpandableSection
      toggleText={`Dashboard containers: ${ready}/${images.length} ready${problems ? `, ${problems} with problems` : ""} (${summary})`}
      isExpanded={expanded}
      onToggle={(_event, value) => setExpanded(value)}
    >
      <Table aria-label="Dashboard container images" variant="compact">
        <Thead><Tr><Th width={25}>Component</Th><Th width={45}>Running</Th><Th width={30}>Status</Th></Tr></Thead>
        <Tbody>
          {images.map(image => {
            const running = runningLabel(image);
            const status = statusOf(image);
            const tag = imageTagOf(image.currentImage);
            return (
              <Tr key={`${image.deployment}/${image.container}`}>
                <Td dataLabel="Component">
                  <strong>{componentName(image.container)}</strong>
                  <Content component="small">{image.deployment}</Content>
                </Td>
                <Td dataLabel="Running">
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                    <FlexItem><Label isCompact color={running.color}>{running.text}</Label></FlexItem>
                    {tag && running.text !== "Release" && <FlexItem><Content component="small"><code>{tag}</code></Content></FlexItem>}
                    {image.flavor && running.text !== "Release" && <FlexItem><Content component="small">{image.flavor === "odh" ? "ODH build" : "RHOAI build"}</Content></FlexItem>}
                  </Flex>
                  <div style={{ maxWidth: "28rem" }}><ImageRef image={image.currentImage} /></div>
                </Td>
                <Td dataLabel="Status">
                  <Label isCompact color={status.kind === "ok" ? "green" : status.kind === "progress" ? "blue" : "red"}
                    icon={status.kind === "ok" ? <CheckCircleIcon /> : status.kind === "progress" ? <InProgressIcon /> : <ExclamationCircleIcon />}>
                    {status.text}
                  </Label>
                  {status.detail && <Content component="small" style={{ overflowWrap: "anywhere" }}>{status.detail}</Content>}
                  {image.podName && status.kind !== "ok" && <Content component="small">Pod: {image.podName}</Content>}
                </Td>
              </Tr>
            );
          })}
        </Tbody>
      </Table>
    </ExpandableSection>
  );
};
