import React, { useState } from "react";
import {
  Alert,
  Button,
  Content,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Link } from "react-router-dom";
import { useFbcContent } from "../hooks/useFbcContent";
import { CategorySelect, ComponentImagesTable } from "./BuildContents";
import { TruncatedText } from "./LongText";

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
        title={data?.bundleName ? `Catalog: ${data.bundleName}` : "Build contents"}
        labelId="fbc-content-title"
        description={data?.tag ? `Tag: ${data.tag}` : undefined}
      />
      <ModalBody>
        <div aria-live="polite">
          {loading && (
            <EmptyState headingLevel="h2" titleText="Reading the catalog" icon={Spinner} variant={EmptyStateVariant.sm}>
              <EmptyStateBody>Downloading and parsing the FBC image; this takes a few seconds.</EmptyStateBody>
            </EmptyState>
          )}
          {error && (
            <Alert component="p" variant="danger" isInline title="Could not read the catalog">
              <TruncatedText>{error.message}</TruncatedText>
            </Alert>
          )}
        </div>

        {data && !loading && (
          <Stack hasGutter>
            {sortedCategories.length > 0 && (
              <StackItem>
                <CategorySelect
                  label="Component category"
                  categories={Object.fromEntries(sortedCategories.map((c) => [c, categories[c]]))}
                  total={images.length}
                  active={category ?? "all"}
                  onSelect={(c) => setActiveCategory(c === "all" ? null : c)}
                />
              </StackItem>
            )}
            <StackItem>
              {filtered.length > 0 ? (
                <ComponentImagesTable label="Component images" images={filtered} labelsPending={!labelsDone} />
              ) : (
                <Content component="p">No component images found in this catalog.</Content>
              )}
            </StackItem>
          </Stack>
        )}
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={onClose}>Close</Button>
        <Button variant="link" component={(props: React.AnchorHTMLAttributes<HTMLAnchorElement>) => <Link {...props} to="/builds" />} onClick={onClose}>
          Open Build Explorer
        </Button>
      </ModalFooter>
    </Modal>
  );
};
