import React, { useState } from "react";
import {
  Button,
  Content,
  Flex,
  FlexItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Skeleton,
  Spinner,
  ToggleGroup,
  ToggleGroupItem,
} from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { Link } from "react-router-dom";
import { formatRelativeTime } from "../utils";
import { useFbcContent } from "../hooks/useFbcContent";
import { ImageRef } from "./ImageRef";
import { CATEGORY_LABELS } from "./BuildContents";

interface FBCContentModalProps {
  image: string;
  isOpen: boolean;
  onClose: () => void;
}

const CATEGORY_ORDER = ["core", "runtime", "workbench", "pipeline", "training", "infra", "other"];

export const FBCContentModal: React.FC<FBCContentModalProps> = ({ image, isOpen, onClose }) => {
  // The hook cancels the requests for a previous image, so labels from one
  // build are never shown for another.
  const { data, loading, labelsDone, error } = useFbcContent(isOpen && image ? image : null);
  const [activeCategory, setActiveCategory] = useState<string | null>(null);

  const images = data?.relatedImages ?? [];
  const categories = data?.categories || {};
  const sortedCategories = CATEGORY_ORDER.filter(c => (categories[c] ?? 0) > 0);
  const category = activeCategory && sortedCategories.includes(activeCategory) ? activeCategory : null;
  const filtered = category ? images.filter(i => i.category === category) : images;

  return (
    <Modal
      aria-labelledby="fbc-content-title"
      variant={ModalVariant.large}
      isOpen={isOpen}
      onClose={onClose}
    >
      <ModalHeader
        title={data?.bundleName ? `Catalog: ${data.bundleName}` : "FBC Catalog Contents"}
        labelId="fbc-content-title"
        description={data?.tag ? `Tag: ${data.tag}` : undefined}
      />
      <ModalBody>
        <div aria-live="polite">
          {loading && (
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }} style={{ padding: "2rem" }}>
              <FlexItem>
                <Spinner size="lg" aria-label="Loading catalog content" />
              </FlexItem>
              <FlexItem>
                <Content component="p">Downloading and parsing FBC catalog image...</Content>
              </FlexItem>
              <FlexItem>
                <Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)" }}>
                  This may take a few seconds depending on image size
                </Content>
              </FlexItem>
            </Flex>
          )}

          {error && (
            <Content component="p" role="alert" style={{ color: "var(--pf-t--global--color--status--danger--default)" }}>
              {error.message}
            </Content>
          )}
        </div>

        {data && !loading && (
          <>
            {sortedCategories.length > 0 && (
              <ToggleGroup aria-label="Component category" isCompact style={{ marginBottom: "1rem", flexWrap: "wrap" }}>
                <ToggleGroupItem
                  text={`All (${images.length})`}
                  isSelected={category === null}
                  onChange={() => setActiveCategory(null)}
                />
                {sortedCategories.map(cat => (
                  <ToggleGroupItem
                    key={cat}
                    text={`${CATEGORY_LABELS[cat] || cat} (${categories[cat]})`}
                    isSelected={category === cat}
                    onChange={() => setActiveCategory(cat)}
                  />
                ))}
              </ToggleGroup>
            )}

            {filtered.length > 0 ? (
              <div style={{ maxHeight: "50vh", overflowY: "auto" }}>
                <Table aria-label="Component images" variant="compact" borders={false}>
                  <Thead>
                    <Tr>
                      <Th>Component</Th>
                      <Th>Commit</Th>
                      <Th>Built</Th>
                      <Th>Version</Th>
                    </Tr>
                  </Thead>
                  <Tbody>
                    {filtered.map((img, idx) => {
                      const shortSha = img.gitCommit?.slice(0, 7);
                      const commitURL = img.gitCommit && img.gitURL ? `${img.gitURL}/commit/${img.gitCommit}` : "";
                      return (
                        <Tr key={`${img.name}-${idx}`}>
                          <Td dataLabel="Component">
                            <ImageRef image={img.image} display={img.name || undefined} isCode={!img.name} what={`image reference of ${img.name || "this component"}`} />
                          </Td>
                          <Td dataLabel="Commit">
                            {shortSha ? (
                              <Button variant="link" isInline component="a" href={commitURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">
                                {shortSha}
                              </Button>
                            ) : labelsDone ? "-" : <Skeleton width="4rem" screenreaderText={`Loading commit of ${img.name}`} />}
                          </Td>
                          <Td dataLabel="Built">
                            {img.buildDate ? (
                              <Content component="small">
                                <time dateTime={img.buildDate} title={new Date(img.buildDate).toLocaleString()}>{formatRelativeTime(img.buildDate)}</time>
                              </Content>
                            ) : labelsDone ? "-" : <Skeleton width="3rem" screenreaderText={`Loading build date of ${img.name}`} />}
                          </Td>
                          <Td dataLabel="Version">
                            {img.version ? <Content component="small">{img.version}</Content> : labelsDone ? "-" : <Skeleton width="3rem" screenreaderText={`Loading version of ${img.name}`} />}
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
              </div>
            ) : (
              <Content component="p">No component images found in this catalog.</Content>
            )}
          </>
        )}
      </ModalBody>
      <ModalFooter>
        <Content component="small" style={{ flex: 1 }}>
          Looking for another build?{" "}
          <Link to="/builds" onClick={onClose}>Go to Build Explorer</Link>
        </Content>
        <Button variant="secondary" onClick={onClose}>Close</Button>
      </ModalFooter>
    </Modal>
  );
};
