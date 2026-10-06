import React, { useState } from "react";
import {
  Alert,
  Button,
  Content,
  Flex,
  FlexItem,
  Label,
  Skeleton,
  ToggleGroup,
  ToggleGroupItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { RelatedImage } from "../types";
import { formatRelativeTime } from "../utils";
import { useFbcContent } from "../hooks/useFbcContent";
import { ImageRef } from "./ImageRef";

export const CATEGORY_LABELS: Record<string, string> = {
  core: "Core",
  runtime: "Runtimes",
  workbench: "Workbenches",
  pipeline: "Pipelines",
  training: "Training",
  infra: "Infra",
  other: "Other",
};

const CATEGORY_ORDER = ["core", "runtime", "workbench", "pipeline", "training", "infra", "other"];

export function categoryOrder(cat: string): number {
  const i = CATEGORY_ORDER.indexOf(cat);
  return i < 0 ? CATEGORY_ORDER.length : i;
}

/** Category filter as a ToggleGroup, so the selection is exposed (aria-pressed), not only coloured (A08-12). */
export const CategoryToggle: React.FC<{
  label: string;
  categories: Record<string, number>;
  total: number;
  active: string;
  onSelect: (cat: string) => void;
}> = ({ label, categories, total, active, onSelect }) => (
  <ToggleGroup aria-label={label} isCompact style={{ flexWrap: "wrap" }}>
    <ToggleGroupItem text={`All (${total})`} isSelected={active === "all"} onChange={() => onSelect("all")} />
    {Object.entries(categories)
      .sort(([a], [b]) => categoryOrder(a) - categoryOrder(b))
      .map(([cat, count]) => (
        <ToggleGroupItem
          key={cat}
          text={`${CATEGORY_LABELS[cat] || cat} (${count})`}
          isSelected={active === cat}
          onChange={() => onSelect(cat)}
        />
      ))}
  </ToggleGroup>
);

/**
 * The component table of one build. Rows appear as soon as the catalog is
 * parsed; Commit, Built and Version fill in when the image labels arrive,
 * with a skeleton per cell meanwhile (A08-9).
 */
export const ComponentImagesTable: React.FC<{
  label: string;
  images: RelatedImage[];
  labelsPending: boolean;
}> = ({ label, images, labelsPending }) => (
  <Table aria-label={label} variant="compact" borders={false}>
    <Thead>
      <Tr>
        <Th width={40}>Component</Th>
        <Th>Commit</Th>
        <Th>Built</Th>
        <Th>Version</Th>
      </Tr>
    </Thead>
    <Tbody>
      {images.map((ri) => {
        const shortSha = ri.gitCommit ? ri.gitCommit.slice(0, 7) : "";
        const commitURL = ri.gitCommit && ri.gitURL ? `${ri.gitURL}/commit/${ri.gitCommit}` : "";
        return (
          <Tr key={ri.name || ri.image}>
            <Td dataLabel="Component">
              <ImageRef image={ri.image} display={ri.name || undefined} isCode={!ri.name} what={`image reference of ${ri.name || "this component"}`} />
            </Td>
            <Td dataLabel="Commit">
              {shortSha ? (
                commitURL ? (
                  <Button variant="link" isInline component="a" href={commitURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm" aria-label={`Commit ${ri.gitCommit} on GitHub`}>
                    <code>{shortSha}</code>
                  </Button>
                ) : <code>{shortSha}</code>
              ) : labelsPending ? <Skeleton width="4rem" screenreaderText={`Loading commit of ${ri.name}`} /> : "-"}
            </Td>
            <Td dataLabel="Built">
              {ri.buildDate ? (
                <Content component="small"><time dateTime={ri.buildDate} title={new Date(ri.buildDate).toLocaleString()}>{formatRelativeTime(ri.buildDate)}</time></Content>
              ) : labelsPending ? <Skeleton width="3rem" screenreaderText={`Loading build date of ${ri.name}`} /> : "-"}
            </Td>
            <Td dataLabel="Version">
              {ri.version || (labelsPending ? <Skeleton width="3rem" screenreaderText={`Loading version of ${ri.name}`} /> : "-")}
            </Td>
          </Tr>
        );
      })}
    </Tbody>
  </Table>
);

/** Contents of one FBC build: bundle, category filter and component table. */
export const BuildContents: React.FC<{ image: string; title: string; defaultCategory?: string }> = ({ image, title, defaultCategory = "core" }) => {
  const { data, loading, labelsLoading, labelsDone, error } = useFbcContent(image);
  const [category, setCategory] = useState(defaultCategory);

  if (loading) {
    return (
      <div aria-busy="true">
        <Content component="small">Downloading and parsing the catalog image...</Content>
        <Skeleton height="1.5rem" screenreaderText="Loading catalog content" />
      </div>
    );
  }
  if (error) {
    return <Alert component="p" variant="danger" title="Could not load the catalog content" isInline isLiveRegion>{error.message}</Alert>;
  }
  if (!data) return null;

  const all = data.relatedImages ?? [];
  const cats = data.categories ?? {};
  const active = category === "all" || cats[category] ? category : "all";
  const shown = active === "all" ? all : all.filter((ri) => ri.category === active);
  return (
    <>
      <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
        {data.bundleName && <FlexItem><Label isCompact color="green">{data.bundleName}</Label></FlexItem>}
        <FlexItem>
          <CategoryToggle label={`Category for ${title}`} categories={cats} total={all.length} active={active} onSelect={setCategory} />
        </FlexItem>
        {labelsLoading && (
          <FlexItem><Content component="small" aria-live="polite">Resolving git commits for {all.length} images...</Content></FlexItem>
        )}
      </Flex>
      {data.error && <Alert component="p" variant="warning" title={data.error} isInline />}
      {shown.length > 0
        ? <ComponentImagesTable label={`Components in ${title}`} images={shown} labelsPending={!labelsDone} />
        : <Content component="p">No component images in this category.</Content>}
    </>
  );
};
