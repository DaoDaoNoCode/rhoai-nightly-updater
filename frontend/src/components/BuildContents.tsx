import React, { useState } from "react";
import {
  Alert,
  Badge,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  MenuToggle,
  Select,
  SelectList,
  SelectOption,
  Skeleton,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { RelatedImage } from "../types";
import { RelativeTime } from "./RelativeTime";
import { TruncatedText } from "./LongText";
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

/** Category filter as a select with counts (PF: select menus for more than a few options). */
export const CategorySelect: React.FC<{
  label: string;
  categories: Record<string, number>;
  total: number;
  active: string;
  onSelect: (cat: string) => void;
}> = ({ label, categories, total, active, onSelect }) => {
  const [open, setOpen] = useState(false);
  const options: [string, string, number][] = [
    ["all", "All categories", total],
    ...Object.entries(categories)
      .sort(([a], [b]) => categoryOrder(a) - categoryOrder(b))
      .map(([cat, count]): [string, string, number] => [cat, CATEGORY_LABELS[cat] || cat, count]),
  ];
  const current = options.find(([id]) => id === active) ?? options[0];
  return (
    <Select
      isOpen={open}
      selected={active}
      onOpenChange={setOpen}
      onSelect={(_e, value) => { onSelect(String(value)); setOpen(false); }}
      toggle={(ref) => (
        <MenuToggle ref={ref} onClick={() => setOpen(!open)} isExpanded={open} aria-label={`${label}: ${current[1]}`} badge={<Badge isRead>{current[2]}</Badge>}>
          {current[1]}
        </MenuToggle>
      )}
      shouldFocusToggleOnSelect
    >
      <SelectList aria-label={label}>
        {options.map(([id, text, count]) => (
          <SelectOption key={id} value={id}>
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "nowrap" }}>
              <FlexItem>{text}</FlexItem>
              <FlexItem><Badge isRead>{count}</Badge></FlexItem>
            </Flex>
          </SelectOption>
        ))}
      </SelectList>
    </Select>
  );
};

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
        <Th modifier="nowrap">Built</Th>
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
                  <a href={commitURL} target="_blank" rel="noopener noreferrer" aria-label={`Commit ${ri.gitCommit} on GitHub`}><code>{shortSha}</code></a>
                ) : <code>{shortSha}</code>
              ) : labelsPending ? <Skeleton width="4rem" screenreaderText={`Loading commit of ${ri.name}`} /> : <Unknown />}
            </Td>
            <Td dataLabel="Built" modifier="nowrap">
              {ri.buildDate ? <RelativeTime date={ri.buildDate} />
                : labelsPending ? <Skeleton width="3rem" screenreaderText={`Loading build date of ${ri.name}`} /> : <Unknown />}
            </Td>
            <Td dataLabel="Version">
              {ri.version || (labelsPending ? <Skeleton width="3rem" screenreaderText={`Loading version of ${ri.name}`} /> : <Unknown />)}
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
      <Stack hasGutter aria-busy="true">
        <StackItem><Content component="p" className="pf-v6-u-text-color-subtle">Downloading and parsing the catalog image...</Content></StackItem>
        <StackItem><Skeleton height="1.5rem" screenreaderText="Loading catalog content" /></StackItem>
        <StackItem><Skeleton height="1.5rem" /></StackItem>
      </Stack>
    );
  }
  if (error) {
    return (
      <Alert component="p" variant="danger" title="Could not load the catalog content" isInline isLiveRegion>
        <TruncatedText>{error.message}</TruncatedText>
      </Alert>
    );
  }
  if (!data) return null;

  const all = data.relatedImages ?? [];
  const cats = data.categories ?? {};
  const active = category === "all" || cats[category] ? category : "all";
  const shown = active === "all" ? all : all.filter((ri) => ri.category === active);
  return (
    <Stack hasGutter>
      <StackItem>
        <Flex gap={{ default: "gapMd" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }} justifyContent={{ default: "justifyContentSpaceBetween" }}>
          <FlexItem>
            <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "14ch" }}>
              <DescriptionListGroup>
                <DescriptionListTerm>Operator bundle</DescriptionListTerm>
                <DescriptionListDescription>{data.bundleName || <Unknown />}</DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </FlexItem>
          <FlexItem>
            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
              {labelsLoading && (
                <FlexItem className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm" aria-live="polite">Resolving git commits for {all.length} images...</FlexItem>
              )}
              <FlexItem>
                <CategorySelect label={`Category for ${title}`} categories={cats} total={all.length} active={active} onSelect={setCategory} />
              </FlexItem>
            </Flex>
          </FlexItem>
        </Flex>
      </StackItem>
      {data.error && <StackItem><Alert component="p" variant="warning" title={data.error} isInline /></StackItem>}
      <StackItem>
        {shown.length > 0
          ? <ComponentImagesTable label={`Components in ${title}`} images={shown} labelsPending={!labelsDone} />
          : <Content component="p">No component images in this category.</Content>}
      </StackItem>
    </Stack>
  );
};

const Unknown: React.FC = () => <span className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm">Unknown</span>;
