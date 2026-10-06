import React, { useState } from "react";
import { Card, CardBody, CardExpandableContent, CardHeader, CardTitle, Content, Flex, FlexItem, Title } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import type { DashboardDevImage } from "../types";
import { ImageRef } from "./ImageRef";
import { StatusLabel, TagLabel } from "./StatusLabel";
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

const RUNNING_COLORS: Record<ReturnType<typeof runningLabel>["color"], "blue" | "purple" | "grey" | "teal"> = {
  blue: "blue", purple: "purple", grey: "grey", orange: "teal",
};

/** Per-container images of the dashboard as an expandable card: what runs where, and why a container is not ready. */
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
        </CardBody>
        <CardBody className="pf-v6-u-px-0">
              <Table aria-label="Dashboard container images" variant="compact" borders={false}>
                <Thead><Tr><Th width={25}>Component</Th><Th width={45}>Running</Th><Th width={30}>Status</Th></Tr></Thead>
                <Tbody>
                  {images.map((image, rowIndex) => {
                    const running = runningLabel(image);
                    const status = statusOf(image);
                    const tag = imageTagOf(image.currentImage);
                    return (
                      <Tr key={`${image.deployment}/${image.container}`} isStriped={rowIndex % 2 === 1}>
                        <Td dataLabel="Component">
                          <strong>{componentName(image.container)}</strong>
                          <div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{image.deployment}</div>
                        </Td>
                        <Td dataLabel="Running">
                          <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} className="pf-v6-u-mb-xs">
                            <FlexItem><TagLabel color={RUNNING_COLORS[running.color]}>{running.text}</TagLabel></FlexItem>
                            {tag && running.text !== "Release" && <FlexItem><code>{tag}</code></FlexItem>}
                            {image.flavor && running.text !== "Release" && <FlexItem className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle">{image.flavor === "odh" ? "ODH build" : "RHOAI build"}</FlexItem>}
                          </Flex>
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
