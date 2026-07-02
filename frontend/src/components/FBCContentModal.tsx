import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Button,
  Content,
  Flex,
  FlexItem,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Tooltip,
} from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { Link } from "react-router-dom";
import type { FBCContentResponse, RelatedImage } from "../types";
import { getBuildExplorerContent } from "../services/api";
import { formatRelativeTime } from "../utils";

interface FBCContentModalProps {
  image: string;
  isOpen: boolean;
  onClose: () => void;
}

const CATEGORY_ORDER = ["core", "runtime", "workbench", "pipeline", "training", "infra", "other"];

const CATEGORY_COLORS: Record<string, "blue" | "teal" | "purple" | "orange" | "grey"> = {
  core: "blue",
  runtime: "teal",
  workbench: "purple",
  pipeline: "orange",
  training: "orange",
  infra: "grey",
  other: "grey",
};

export const FBCContentModal: React.FC<FBCContentModalProps> = ({ image, isOpen, onClose }) => {
  const [data, setData] = useState<FBCContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [labelsData, setLabelsData] = useState<RelatedImage[] | null>(null);
  const [activeCategory, setActiveCategory] = useState<string | null>(null);
  const prevImageRef = useRef("");

  const fetchContent = useCallback(async (img: string) => {
    setLoading(true);
    setError("");
    setData(null);
    setLabelsData(null);
    setActiveCategory(null);
    try {
      const result = await getBuildExplorerContent(img);
      setData(result);
      if (result.relatedImages?.length > 0) {
        getBuildExplorerContent(img, true)
          .then(enriched => setLabelsData(enriched.relatedImages))
          .catch(() => {});
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load catalog content");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (isOpen && image && image !== prevImageRef.current) {
      prevImageRef.current = image;
      fetchContent(image);
    }
    if (!isOpen) {
      prevImageRef.current = "";
    }
  }, [isOpen, image, fetchContent]);

  const images = labelsData || data?.relatedImages || [];
  const categories = data?.categories || {};
  const sortedCategories = CATEGORY_ORDER.filter(c => (categories[c] ?? 0) > 0);
  const filtered = activeCategory ? images.filter(i => i.category === activeCategory) : images;

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
          <Content component="p" style={{ color: "var(--pf-t--global--color--status--danger--default)" }}>
            {error}
          </Content>
        )}

        {data && !loading && (
          <>
            {sortedCategories.length > 0 && (
              <Flex gap={{ default: "gapSm" }} style={{ marginBottom: "1rem" }} flexWrap={{ default: "wrap" }}>
                <FlexItem>
                  <Label
                    isCompact
                    color={activeCategory === null ? "blue" : "grey"}
                    onClick={() => setActiveCategory(null)}
                    style={{ cursor: "pointer" }}
                  >
                    All ({images.length})
                  </Label>
                </FlexItem>
                {sortedCategories.map(cat => (
                  <FlexItem key={cat}>
                    <Label
                      isCompact
                      color={activeCategory === cat ? CATEGORY_COLORS[cat] : "grey"}
                      onClick={() => setActiveCategory(activeCategory === cat ? null : cat)}
                      style={{ cursor: "pointer" }}
                    >
                      {cat} ({categories[cat]})
                    </Label>
                  </FlexItem>
                ))}
              </Flex>
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
                            <Tooltip content={img.image}>
                              <Content component="small">{img.name}</Content>
                            </Tooltip>
                          </Td>
                          <Td dataLabel="Commit">
                            {shortSha ? (
                              <Button variant="link" isInline component="a" href={commitURL} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm">
                                {shortSha}
                              </Button>
                            ) : labelsData ? "-" : <Spinner size="sm" />}
                          </Td>
                          <Td dataLabel="Built">
                            {img.buildDate ? (
                              <Tooltip content={new Date(img.buildDate).toLocaleString()}>
                                <Content component="small">{formatRelativeTime(img.buildDate)}</Content>
                              </Tooltip>
                            ) : labelsData ? "-" : <Spinner size="sm" />}
                          </Td>
                          <Td dataLabel="Version">
                            <Content component="small">{img.version || "-"}</Content>
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
