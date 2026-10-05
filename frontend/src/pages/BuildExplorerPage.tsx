import React, { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  Alert,
  Badge,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Label,
  MenuToggle,
  PageSection,
  SearchInput,
  Select,
  SelectList,
  SelectOption,
  Spinner,
  Title,
  ToggleGroup,
  ToggleGroupItem,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  Tooltip,
  Flex,
  FlexItem,
} from "@patternfly/react-core";
import {
  Table,
  Thead,
  Tbody,
  Tr,
  Th,
  Td,
  ExpandableRowContent,
} from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import type { NightlyTag, FBCContentResponse } from "../types";
import { ApiError, getBuildExplorerTags, getBuildExplorerContent, toApiError } from "../services/api";
import { formatRelativeTime, truncateImage } from "../utils";
import { ErrorAlert } from "../components/ErrorAlert";
import { PageHeader } from "../components/PageHeader";
import { CopyableText } from "../components/CopyableText";
import { useFbcContent } from "../hooks/useFbcContent";

const CATEGORY_LABELS: Record<string, string> = {
  core: "Core",
  runtime: "Runtimes",
  workbench: "Workbenches",
  pipeline: "Pipelines",
  training: "Training",
  infra: "Infra",
  other: "Other",
};

/** Category filter as a ToggleGroup, so the selection is exposed (aria-pressed), not only coloured. */
const CategoryToggle: React.FC<{
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

function categoryOrder(cat: string): number {
  const order: Record<string, number> = {
    core: 0,
    runtime: 1,
    workbench: 2,
    pipeline: 3,
    training: 4,
    infra: 5,
  };
  return order[cat] ?? 6;
}

function parseTagVersion(
  tag: string,
): { series: string; major: number; minor: number; ea: number } | null {
  const m = tag.match(/^rhoai-(\d+)\.(\d+)(?:\.\d+)?(-ea(?:\.(\d+))?)?$/);
  if (!m) return null;
  return {
    series: `${m[1]}.${m[2]}`,
    major: parseInt(m[1], 10),
    minor: parseInt(m[2], 10),
    ea: m[3] ? (m[4] ? parseInt(m[4], 10) : 0) : -1,
  };
}

/** Semantic compare for parsed tags: higher versions first (descending). */
function compareTagsDesc(
  a: { major: number; minor: number; ea: number },
  b: { major: number; minor: number; ea: number },
): number {
  if (a.major !== b.major) return b.major - a.major;
  if (a.minor !== b.minor) return b.minor - a.minor;
  // GA (ea=-1) sorts before any EA in descending order (GA > EA)
  if (a.ea === -1 && b.ea === -1) return 0;
  if (a.ea === -1) return -1; // a is GA, a comes first
  if (b.ea === -1) return 1; // b is GA, b comes first
  return b.ea - a.ea;
}

export const BuildExplorerPage: React.FC = () => {
  const [searchParams] = useSearchParams();
  const [tags, setTags] = useState<NightlyTag[]>([]);
  const [loading, setLoading] = useState(true);
  const [datesLoading, setDatesLoading] = useState(false);
  const tagsRequestId = useRef(0);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  const [expandedRows, setExpandedRows] = useState<Record<string, boolean>>({});
  const [fbcContent, setFbcContent] = useState<
    Record<string, FBCContentResponse>
  >({});
  const [fbcLoading, setFbcLoading] = useState<Record<string, boolean>>({});
  const [fbcLabelsLoading, setFbcLabelsLoading] = useState<
    Record<string, boolean>
  >({});
  const [fbcError, setFbcError] = useState<Record<string, string>>({});

  const [versionFilter, setVersionFilter] = useState("all");
  const [typeFilter, setTypeFilter] = useState("all");
  const [versionSelectOpen, setVersionSelectOpen] = useState(false);
  const [categoryFilter, setCategoryFilter] = useState<Record<string, string>>(
    {},
  );

  // Custom image lookup. useFbcContent cancels the lookup of a previous
  // image, so a slow earlier lookup can't replace the current one.
  const [searchImage, setSearchImage] = useState("");
  const [searched, setSearched] = useState<{ image: string; nonce: number } | null>(null);
  const searchResult = useFbcContent(searched?.image ?? null, searched?.nonce);
  const searchContent = searchResult.data;
  const searchLoading = searchResult.loading;
  const searchLabelsLoading = searchResult.labelsLoading;
  const searchError = searchResult.error?.message ?? null;

  // Auto-populate from URL param ?image=...
  const urlImageApplied = useRef(false);

  const handleSearchSubmit = () => {
    const img = searchImage.trim();
    if (!img) return;
    setSearched((prev) => ({ image: img, nonce: (prev?.nonce ?? 0) + 1 }));
  };

  const fetchTags = useCallback(async () => {
    const requestId = ++tagsRequestId.current;
    setLoading(true);
    setDatesLoading(false);
    setError(null);
    try {
      const res = await getBuildExplorerTags();
      if (requestId !== tagsRequestId.current) return;
      setTags(res.tags || []);
      setLastRefreshed(new Date());
      setDatesLoading(true);
      // Show versions immediately; image config timestamps are optional and
      // can arrive later. Only merge dates for the exact image already shown.
      void getBuildExplorerTags(true)
        .then((enriched) => {
          if (requestId !== tagsRequestId.current) return;
          const dates = new Map((enriched.tags ?? []).map((tag) => [tag.image, tag.buildDate]));
          setTags((current) => current.map((tag) => ({
            ...tag,
            buildDate: dates.get(tag.image) || tag.buildDate,
          })));
        })
        .catch(() => { /* Build dates are optional; keep the version list usable. */ })
        .finally(() => {
          if (requestId === tagsRequestId.current) setDatesLoading(false);
        });
    } catch (e) {
      if (requestId === tagsRequestId.current) {
        setError(toApiError(e, "Failed to fetch tags"));
      }
    } finally {
      if (requestId === tagsRequestId.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchTags();
    const requests = tagsRequestId;
    // Invalidate in-flight tag requests when the page unmounts.
    return () => { requests.current++; };
  }, [fetchTags]);

  useEffect(() => {
    document.title = "Build Explorer — RHOAI Nightly Updater";
  }, []);

  // If ?image= is present in the URL, auto-fill the search and trigger lookup
  useEffect(() => {
    if (urlImageApplied.current) return;
    const urlImage = searchParams.get("image");
    if (urlImage) {
      urlImageApplied.current = true;
      setSearchImage(urlImage);
      setSearched({ image: urlImage, nonce: 1 });
    }
  }, [searchParams]);

  const versionSeries = React.useMemo(() => {
    const majors = new Set<string>();
    for (const t of tags) {
      const parsed = parseTagVersion(t.tag);
      if (parsed) majors.add(parsed.series.split(".")[0]);
    }
    return Array.from(majors)
      .sort((a, b) => parseInt(b, 10) - parseInt(a, 10));
  }, [tags]);

  const filteredTags = React.useMemo(() => {
    return tags
      .filter((t) => {
        const parsed = parseTagVersion(t.tag);
        if (!parsed) return false;
        if (
          versionFilter !== "all" &&
          !parsed.series.startsWith(versionFilter + ".")
        )
          return false;
        if (typeFilter === "ea" && parsed.ea < 0) return false;
        if (typeFilter === "ga" && parsed.ea >= 0) return false;
        return true;
      })
      .sort((a, b) => {
        const pa = parseTagVersion(a.tag);
        const pb = parseTagVersion(b.tag);
        if (!pa || !pb) return 0;
        return compareTagsDesc(pa, pb);
      });
  }, [tags, versionFilter, typeFilter]);

  const handleToggleExpand = async (tag: NightlyTag) => {
    const key = tag.tag;
    const isExpanding = !expandedRows[key];
    setExpandedRows((prev) => ({ ...prev, [key]: isExpanding }));

    if (isExpanding && !fbcContent[key] && !fbcLoading[key]) {
      setFbcLoading((prev) => ({ ...prev, [key]: true }));
      setFbcError((prev) => ({ ...prev, [key]: "" }));
      try {
        const content = await getBuildExplorerContent(tag.image);
        setFbcContent((prev) => ({ ...prev, [key]: content }));

        // Phase 2: fetch labels in background
        if (content.relatedImages && content.relatedImages.length > 0) {
          setFbcLabelsLoading((prev) => ({ ...prev, [key]: true }));
          try {
            const enriched = await getBuildExplorerContent(tag.image, true);
            setFbcContent((prev) => ({ ...prev, [key]: enriched }));
          } catch {
            // Labels are optional
          } finally {
            setFbcLabelsLoading((prev) => ({ ...prev, [key]: false }));
          }
        }
      } catch (e) {
        setFbcError((prev) => ({
          ...prev,
          [key]: toApiError(e, "Failed to load content").message,
        }));
      } finally {
        setFbcLoading((prev) => ({ ...prev, [key]: false }));
      }
    }
  };

  // The backend answers every tag-listing failure with 500 "failed to fetch
  // nightly tags" and no specific errorCode yet; match that exact response.
  const tagsUnavailable = !!error && error.status === 500 && error.message === "failed to fetch nightly tags";

  return (
    <>
      <PageHeader
        title="Build Explorer"
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={fetchTags}
      />

      {error && !tagsUnavailable && <ErrorAlert error={error} genericTitle="Failed to load tags" />}

      {tagsUnavailable && (
        <PageSection>
          <Alert variant="info" title="Pull secret not configured" isInline component="p">
            <p>The build explorer needs a pull secret to access nightly builds on Quay.io.</p>
            <p style={{ marginTop: "0.5rem" }}>
              Go to the <Link to="/">Dashboard</Link> and configure the <strong>Pull Secret</strong> first.
            </p>
          </Alert>
        </PageSection>
      )}

      {/* Search any FBC image by tag@sha256 */}
      <PageSection>
        <Card isCompact>
          <CardBody>
            <Flex
              alignItems={{ default: "alignItemsFlexEnd" }}
              gap={{ default: "gapSm" }}
            >
              <FlexItem grow={{ default: "grow" }}>
                <Content component="small" style={{ marginBottom: "0.25rem" }}>
                  Look up a specific build by image reference
                </Content>
                <SearchInput
                  placeholder="quay.io/rhoai/rhoai-fbc-fragment:<tag>@sha256:<digest>"
                  value={searchImage}
                  onChange={(_e, val) => setSearchImage(val)}
                  onSearch={handleSearchSubmit}
                  onClear={() => {
                    setSearchImage("");
                    setSearched(null);
                  }}
                  isDisabled={searchLoading}
                  aria-label="FBC image reference"
                />
              </FlexItem>
            </Flex>

            {searchError && (
              <Alert
                variant="danger"
                title="Lookup failed"
                isInline
                isPlain
                isLiveRegion
                component="p"
                style={{ marginTop: "0.5rem" }}
              >
                {searchError}
              </Alert>
            )}

            {searchLoading && (
              <Flex
                gap={{ default: "gapSm" }}
                alignItems={{ default: "alignItemsCenter" }}
                style={{ marginTop: "0.5rem" }}
              >
                <FlexItem>
                  <Spinner size="md" aria-label="Loading" />
                </FlexItem>
                <FlexItem>Downloading and parsing catalog image...</FlexItem>
              </Flex>
            )}

            {searchContent &&
              (() => {
                const allImages = searchContent.relatedImages || [];
                const cats = searchContent.categories || {};
                const activeCat = categoryFilter["__search__"] || "core";
                const displayImages = activeCat === "all" ? allImages : allImages.filter(
                  (ri) => ri.category === activeCat,
                );

                return (
                  <div style={{ marginTop: "0.75rem" }}>
                    <Flex
                      gap={{ default: "gapSm" }}
                      alignItems={{ default: "alignItemsCenter" }}
                      flexWrap={{ default: "wrap" }}
                    >
                      {searchContent.bundleName && (
                        <FlexItem>
                          <Label isCompact color="green">
                            {searchContent.bundleName}
                          </Label>
                        </FlexItem>
                      )}
                      <FlexItem>
                        <CategoryToggle
                          label="Lookup result category"
                          categories={cats}
                          total={allImages.length}
                          active={activeCat}
                          onSelect={(cat) => setCategoryFilter((prev) => ({ ...prev, ["__search__"]: cat }))}
                        />
                      </FlexItem>
                      {searchLabelsLoading && (
                        <FlexItem>
                          <Spinner size="sm" aria-label="Loading labels" />{" "}
                          Resolving git info...
                        </FlexItem>
                      )}
                    </Flex>

                    {displayImages.length > 0 && (
                      <Table
                        aria-label="Lookup results"
                        variant="compact"
                        borders={false}
                      >
                        <Thead>
                          <Tr>
                            <Th>Component</Th>
                            <Th>Commit</Th>
                            <Th>Built</Th>
                            <Th>Version</Th>
                          </Tr>
                        </Thead>
                        <Tbody>
                          {displayImages.map((ri) => {
                            const shortSha = ri.gitCommit
                              ? ri.gitCommit.slice(0, 7)
                              : "";
                            const commitURL =
                              ri.gitCommit && ri.gitURL
                                ? `${ri.gitURL}/commit/${ri.gitCommit}`
                                : "";
                            const compareURL =
                              ri.gitCommit && ri.gitURL
                                ? `${ri.gitURL}/compare/${ri.gitCommit}...main`
                                : "";
                            return (
                              <Tr key={ri.image}>
                                <Td dataLabel="Component">
                                  <CopyableText text={ri.name || truncateImage(ri.image)} value={ri.image} what="image reference" />
                                </Td>
                                <Td dataLabel="Commit">
                                  {searchLabelsLoading && !shortSha ? (
                                    <Spinner size="sm" aria-label="Loading" />
                                  ) : shortSha ? (
                                    <Flex
                                      spaceItems={{ default: "spaceItemsSm" }}
                                      alignItems={{
                                        default: "alignItemsCenter",
                                      }}
                                      flexWrap={{ default: "nowrap" }}
                                    >
                                      <FlexItem>
                                        <Tooltip content={ri.gitCommit}>
                                          <Button
                                            variant="link"
                                            isInline
                                            component="a"
                                            href={commitURL}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            icon={<ExternalLinkAltIcon />}
                                            iconPosition="end"
                                            size="sm"
                                          >
                                            {shortSha}
                                          </Button>
                                        </Tooltip>
                                      </FlexItem>
                                      {compareURL && (
                                        <FlexItem>
                                          <Button
                                            variant="link"
                                            isInline
                                            component="a"
                                            href={compareURL}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            icon={<ExternalLinkAltIcon />}
                                            iconPosition="end"
                                            size="sm"
                                          >
                                            diff
                                          </Button>
                                        </FlexItem>
                                      )}
                                    </Flex>
                                  ) : (
                                    "-"
                                  )}
                                </Td>
                                <Td dataLabel="Built">
                                  {searchLabelsLoading && !ri.buildDate ? (
                                    <Spinner size="sm" aria-label="Loading" />
                                  ) : ri.buildDate ? (
                                    <Content component="small"><time dateTime={ri.buildDate} title={new Date(ri.buildDate).toLocaleString()}>{formatRelativeTime(ri.buildDate)}</time></Content>
                                  ) : (
                                    "-"
                                  )}
                                </Td>
                                <Td dataLabel="Version">{ri.version || "-"}</Td>
                              </Tr>
                            );
                          })}
                        </Tbody>
                      </Table>
                    )}
                  </div>
                );
              })()}
          </CardBody>
        </Card>
      </PageSection>

      {loading && !tags.length && (
        <PageSection>
          <Bullseye>
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              <FlexItem><Spinner size="xl" aria-label="Loading tags" /></FlexItem>
              <FlexItem><Content component="p">Fetching nightly build tags from Quay registry...</Content></FlexItem>
            </Flex>
          </Bullseye>
        </PageSection>
      )}

      {tags.length > 0 && (
        <>
          <PageSection>
            <Toolbar>
              <ToolbarContent>
                <ToolbarItem>
                  <Select
                    isOpen={versionSelectOpen}
                    onOpenChange={setVersionSelectOpen}
                    onSelect={(_e, val) => {
                      setVersionFilter(val as string);
                      setVersionSelectOpen(false);
                    }}
                    selected={versionFilter}
                    toggle={(toggleRef) => (
                      <MenuToggle
                        ref={toggleRef}
                        onClick={() => setVersionSelectOpen(!versionSelectOpen)}
                        isExpanded={versionSelectOpen}
                      >
                        {versionFilter === "all"
                          ? "All versions"
                          : `v${versionFilter}.x`}
                      </MenuToggle>
                    )}
                  >
                    <SelectList aria-label="Version filter">
                      <SelectOption value="all">All versions</SelectOption>
                      {versionSeries.map((s) => (
                        <SelectOption key={s} value={s}>
                          v{s}.x
                        </SelectOption>
                      ))}
                    </SelectList>
                  </Select>
                </ToolbarItem>
                <ToolbarItem>
                  <ToggleGroup aria-label="Type filter">
                    <ToggleGroupItem
                      text="All"
                      isSelected={typeFilter === "all"}
                      onChange={() => setTypeFilter("all")}
                    />
                    <ToggleGroupItem
                      text="EA"
                      isSelected={typeFilter === "ea"}
                      onChange={() => setTypeFilter("ea")}
                    />
                    <ToggleGroupItem
                      text="GA"
                      isSelected={typeFilter === "ga"}
                      onChange={() => setTypeFilter("ga")}
                    />
                  </ToggleGroup>
                </ToolbarItem>
                <ToolbarItem style={{ alignSelf: "center" }}>
                  <Badge isRead>
                    {filteredTags.length} of {tags.length} tags
                  </Badge>
                </ToolbarItem>
              </ToolbarContent>
            </Toolbar>
          </PageSection>

          <PageSection>
            <Card>
              <CardTitle>
                <Title headingLevel="h3">Latest Builds</Title>
              </CardTitle>
              <CardBody>
                <Table aria-label="Build explorer" variant="compact">
                  <Thead>
                    <Tr>
                      <Th screenReaderText="Expand" />
                      <Th>Tag</Th>
                      <Th>Type</Th>
                      <Th>Last Built</Th>
                      <Th>Image</Th>
                    </Tr>
                  </Thead>
                  {filteredTags.map((tag, rowIndex) => {
                    const key = tag.tag;
                    const isExpanded = !!expandedRows[key];
                    const parsed = parseTagVersion(tag.tag);
                    const isEA = parsed && parsed.ea >= 0;
                    const content = fbcContent[key];
                    const isLoadingContent = !!fbcLoading[key];
                    const isLoadingLabels = !!fbcLabelsLoading[key];
                    const contentError = fbcError[key];

                    return (
                      <Tbody key={key} isExpanded={isExpanded}>
                        <Tr>
                          <Td
                            expand={{
                              rowIndex,
                              isExpanded,
                              onToggle: () => handleToggleExpand(tag),
                            }}
                          />
                          <Td dataLabel="Tag" id={`simple-node${rowIndex}`} modifier="nowrap">
                            <strong>{tag.tag}</strong>
                          </Td>
                          <Td dataLabel="Type">
                            <Label isCompact color={isEA ? "orange" : "green"}>
                              {isEA ? (tag.tag.endsWith("-ea") ? "EA" : `EA ${parsed?.ea}`) : "GA"}
                            </Label>
                          </Td>
                          <Td dataLabel="Last Built">
                            {datesLoading && !tag.buildDate ? (
                              <Spinner size="sm" aria-label="Loading build date" />
                            ) : tag.buildDate ? (
                              <Content component="small"><time dateTime={tag.buildDate} title={new Date(tag.buildDate).toLocaleString()}>{formatRelativeTime(tag.buildDate)}</time></Content>
                            ) : (
                              <Content component="small">-</Content>
                            )}
                          </Td>
                          <Td dataLabel="Image">
                            <Content component="small">
                              <CopyableText text={truncateImage(tag.image)} value={tag.image} what="image reference" code />
                            </Content>
                          </Td>
                        </Tr>
                        <Tr isExpanded={isExpanded}>
                          <Td colSpan={5}>
                            <ExpandableRowContent>
                              {isLoadingContent && !content && (
                                <Bullseye>
                                  <Flex
                                    alignItems={{
                                      default: "alignItemsCenter",
                                    }}
                                    gap={{ default: "gapSm" }}
                                  >
                                    <FlexItem>
                                      <Spinner
                                        size="md"
                                        aria-label="Loading FBC content"
                                      />
                                    </FlexItem>
                                    <FlexItem>
                                      Downloading and parsing catalog image...
                                    </FlexItem>
                                  </Flex>
                                </Bullseye>
                              )}

                              {contentError && (
                                <Alert
                                  variant="danger"
                                  title="Failed to load catalog content"
                                  isInline
                                >
                                  {contentError}
                                </Alert>
                              )}

                              {content &&
                                (() => {
                                  const allImages = content.relatedImages || [];
                                  const cats = content.categories || {};
                                  const activeCat =
                                    categoryFilter[key] || "core";
                                  const displayImages = activeCat === "all" ? allImages : allImages.filter(
                                    (ri) => ri.category === activeCat,
                                  );

                                  return (
                                    <>
                                      <Flex
                                        gap={{ default: "gapSm" }}
                                        alignItems={{
                                          default: "alignItemsCenter",
                                        }}
                                        flexWrap={{ default: "wrap" }}
                                      >
                                        {content.bundleName && (
                                          <FlexItem>
                                            <Label isCompact color="green">
                                              {content.bundleName}
                                            </Label>
                                          </FlexItem>
                                        )}
                                        <FlexItem>
                                          <CategoryToggle
                                            label={`Category for ${tag.tag}`}
                                            categories={cats}
                                            total={allImages.length}
                                            active={activeCat}
                                            onSelect={(cat) => setCategoryFilter((prev) => ({ ...prev, [key]: cat }))}
                                          />
                                        </FlexItem>
                                      </Flex>

                                      {isLoadingLabels && (
                                        <Flex alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} style={{ marginTop: "0.5rem" }}>
                                          <FlexItem><Spinner size="sm" aria-label="Loading labels" /></FlexItem>
                                          <FlexItem><Content component="small" style={{ color: "var(--pf-t--global--text--color--subtle)" }}>Resolving git commit info...</Content></FlexItem>
                                        </Flex>
                                      )}

                                      {displayImages.length > 0 && (
                                        <Table
                                          aria-label={`Components in ${tag.tag}`}
                                          variant="compact"
                                          borders={false}
                                        >
                                          <Thead>
                                            <Tr>
                                              <Th>Component</Th>
                                              {!isLoadingLabels && <Th>Commit</Th>}
                                              {!isLoadingLabels && <Th>Built</Th>}
                                              {!isLoadingLabels && <Th>Version</Th>}
                                            </Tr>
                                          </Thead>
                                          <Tbody>
                                            {displayImages.map((ri) => {
                                              const shortSha = ri.gitCommit
                                                ? ri.gitCommit.slice(0, 7)
                                                : "";
                                              const commitURL =
                                                ri.gitCommit && ri.gitURL
                                                  ? `${ri.gitURL}/commit/${ri.gitCommit}`
                                                  : "";
                                              const compareURL =
                                                ri.gitCommit && ri.gitURL
                                                  ? `${ri.gitURL}/compare/${ri.gitCommit}...main`
                                                  : "";

                                              return (
                                                <Tr key={ri.image}>
                                                  <Td dataLabel="Component">
                                                    <CopyableText text={ri.name || truncateImage(ri.image)} value={ri.image} what="image reference" />
                                                  </Td>
                                                  {!isLoadingLabels && (
                                                  <Td dataLabel="Commit">
                                                    {shortSha ? (
                                                      <Flex
                                                        spaceItems={{
                                                          default:
                                                            "spaceItemsSm",
                                                        }}
                                                        alignItems={{
                                                          default:
                                                            "alignItemsCenter",
                                                        }}
                                                        flexWrap={{
                                                          default: "nowrap",
                                                        }}
                                                      >
                                                        <FlexItem>
                                                          <Tooltip
                                                            content={
                                                              ri.gitCommit
                                                            }
                                                          >
                                                            <Button
                                                              variant="link"
                                                              isInline
                                                              component="a"
                                                              href={commitURL}
                                                              target="_blank"
                                                              rel="noopener noreferrer"
                                                              icon={
                                                                <ExternalLinkAltIcon />
                                                              }
                                                              iconPosition="end"
                                                              size="sm"
                                                            >
                                                              {shortSha}
                                                            </Button>
                                                          </Tooltip>
                                                        </FlexItem>
                                                        {compareURL && (
                                                          <FlexItem>
                                                            <Button
                                                              variant="link"
                                                              isInline
                                                              component="a"
                                                              href={compareURL}
                                                              target="_blank"
                                                              rel="noopener noreferrer"
                                                              icon={
                                                                <ExternalLinkAltIcon />
                                                              }
                                                              iconPosition="end"
                                                              size="sm"
                                                            >
                                                              diff
                                                            </Button>
                                                          </FlexItem>
                                                        )}
                                                      </Flex>
                                                    ) : (
                                                      "-"
                                                    )}
                                                  </Td>
                                                  )}
                                                  {!isLoadingLabels && (
                                                  <Td dataLabel="Built">
                                                    {ri.buildDate ? (
                                                      <Content component="small"><time dateTime={ri.buildDate} title={new Date(ri.buildDate).toLocaleString()}>{formatRelativeTime(ri.buildDate)}</time></Content>
                                                    ) : (
                                                      "-"
                                                    )}
                                                  </Td>
                                                  )}
                                                  {!isLoadingLabels && (
                                                  <Td dataLabel="Version">
                                                    {ri.version || "-"}
                                                  </Td>
                                                  )}
                                                </Tr>
                                              );
                                            })}
                                          </Tbody>
                                        </Table>
                                      )}

                                      {content.error && (
                                        <Alert
                                          variant="warning"
                                          title={content.error}
                                          isInline
                                        />
                                      )}
                                    </>
                                  );
                                })()}
                            </ExpandableRowContent>
                          </Td>
                        </Tr>
                      </Tbody>
                    );
                  })}
                </Table>
              </CardBody>
            </Card>
          </PageSection>
        </>
      )}
    </>
  );
};
