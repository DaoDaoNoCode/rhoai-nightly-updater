import React, { useState } from "react";
import { Content, ExpandableSection, Flex, FlexItem, Label, Tooltip } from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import type { DashboardDevImage } from "../types";
import { CopyableText } from "./CopyableText";

function componentName(container: string): string {
  if (container === "rhods-dashboard") return "Dashboard";
  if (container === "core-bff") return "Core BFF";
  const acronyms: Record<string, string> = { automl: "AutoML", autorag: "AutoRAG", maas: "MaaS", mlflow: "MLflow", ai: "AI" };
  return container.replace(/-ui$/, "").split("-").map(word => acronyms[word] || word[0]?.toUpperCase() + word.slice(1)).join(" ");
}

function imageParts(image: string): { repository: string; tag: string; digest?: string } {
  const [reference, digest] = image.split("@", 2);
  const separator = reference.lastIndexOf(":");
  return separator > reference.lastIndexOf("/")
    ? { repository: reference.slice(0, separator), tag: reference.slice(separator + 1), digest }
    : { repository: reference, tag: "", digest };
}

export const DashboardImages: React.FC<{ images: DashboardDevImage[] }> = ({ images }) => {
  const [expanded, setExpanded] = useState(false);
  const updated = images.filter(image => image.matchesTarget !== false).length;
  const ready = images.filter(image => image.ready && image.matchesTarget !== false).length;

  return (
    <ExpandableSection
      toggleText={`Dashboard components (${ready}/${images.length} ready${updated < images.length ? `, ${images.length - updated} not updated` : ""})`}
      isExpanded={expanded}
      onToggle={(_event, value) => setExpanded(value)}
    >
      <Table aria-label="Dashboard component images" variant="compact" role="table">
        <Thead><Tr><Th width={20}>Component</Th><Th width={60}>Build</Th><Th width={20}>Status</Th></Tr></Thead>
        <Tbody>
          {images.map(image => {
            const current = imageParts(image.currentImage);
            const notUpdated = image.matchesTarget === false;
            const build = current.tag || (image.currentImage === image.defaultImage ? "Installed release" : "Digest pinned");
            return (
              <Tr key={`${image.deployment}/${image.container}`}>
                <Td dataLabel="Component">
                  <strong>{componentName(image.container)}</strong>
                  <Content component="small">{image.deployment}</Content>
                </Td>
                <Td dataLabel="Build">
                  <Content component="small" className="pf-v6-u-text-break-word">
                    <Tooltip content={image.currentImage}>
                      {current.repository.startsWith("quay.io/") ? (
                        <a href={`https://quay.io/repository/${current.repository.slice("quay.io/".length)}`} target="_blank" rel="noopener noreferrer">{current.repository.slice("quay.io/".length)}</a>
                      ) : <span>{current.repository}</span>}
                    </Tooltip>
                  </Content>
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} className="pf-v6-u-mt-xs">
                    <FlexItem><Label isCompact color={current.tag === "main" || current.tag.startsWith("pr-") ? "blue" : "grey"}>{build}</Label></FlexItem>
                    {current.digest && <FlexItem><Content component="small"><CopyableText text={`${current.digest.slice(0, 19)}…`} value={current.digest} what="image digest" code /></Content></FlexItem>}
                  </Flex>
                </Td>
                <Td dataLabel="Status">
                  <Label isCompact color={notUpdated || !image.ready ? "orange" : "green"}>{notUpdated ? "Not updated" : image.ready ? "Ready" : "Rolling out"}</Label>
                  {notUpdated && image.targetImage && <Content component="small" className="pf-v6-u-mt-xs">Target: {imageParts(image.targetImage).tag || "selected build"}</Content>}
                </Td>
              </Tr>
            );
          })}
        </Tbody>
      </Table>
    </ExpandableSection>
  );
};
