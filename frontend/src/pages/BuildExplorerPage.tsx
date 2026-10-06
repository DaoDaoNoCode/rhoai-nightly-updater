import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Flex,
  FlexItem,
  MenuToggle,
  PageSection,
  Popover,
  SearchInput,
  Select,
  SelectList,
  SelectOption,
  Skeleton,
  Spinner,
  Stack,
  StackItem,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarGroup,
  ToolbarItem,
  ToolbarToggleGroup,
} from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td, ExpandableRowContent, type IAction } from "@patternfly/react-table";
import FilterIcon from "@patternfly/react-icons/dist/esm/icons/filter-icon";
import HelpIcon from "@patternfly/react-icons/dist/esm/icons/help-icon";
import type { NightlyTag } from "../types";
import { ApiError, getBuildExplorerTags, toApiError } from "../services/api";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { PageErrorState, PageLoading } from "../components/PageStates";
import { NotRecorded, RelativeTime } from "../components/RelativeTime";
import { RowActions } from "../components/RowActions";
import { TagLabel } from "../components/StatusLabel";
import { ImageRef } from "../components/ImageRef";
import { BuildContents } from "../components/BuildContents";
import { BuildCompare, type BuildSide } from "../components/BuildCompare";
import { classifySearch, imageDigest, installedBuild, shortBuildRef, type InstalledBuild } from "../components/buildDiff";
import { useClusterStatus } from "../state/AppState";
import { useCommitSearch, type SearchBuild } from "../hooks/useCommitSearch";
import { usePRSearch } from "../hooks/usePRSearch";
import { PRSearchResults } from "../components/PRSearchResults";

export function parseTagVersion(
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
  if (a.ea === -1) return -1;
  if (b.ea === -1) return 1;
  return b.ea - a.ea;
}

const INSTALLED_LABEL = "Installed";

const TYPE_LABELS: Record<string, string> = { all: "All types", ea: "EA builds", ga: "GA builds" };

/** Copy a value; the clipboard may be blocked, which is not an error worth showing. */
function copy(text: string): void {
  void navigator.clipboard?.writeText(text).catch(() => {});
}

/** The installed build, under the page description (B5: status.nightly). */
const InstalledBuildDetails: React.FC<{
  installed: InstalledBuild | null;
  statusLoaded: boolean;
  latest?: { image: string; buildDate?: string };
  updateAvailable?: boolean;
}> = ({ installed, statusLoaded, latest, updateAvailable }) => {
  if (!statusLoaded) return <Skeleton width="24rem" screenreaderText="Loading the installed build" />;
  if (!installed) {
    return (
      <Content component="p" className="pf-v6-u-text-color-subtle">
        RHOAI on this cluster was not installed from a nightly build, so there is no installed build to mark or compare with.
      </Content>
    );
  }
  return (
    <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "14ch" }} aria-label="Installed build">
      <DescriptionListGroup>
        <DescriptionListTerm>Installed build</DescriptionListTerm>
        <DescriptionListDescription>
          <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }} flexWrap={{ default: "wrap" }}>
            <FlexItem><strong>{installed.tag}</strong></FlexItem>
            <FlexItem><ImageRef image={installed.image} display={installed.digest.replace("sha256:", "").slice(0, 12)} what="installed image reference" /></FlexItem>
            {installed.buildDate && <FlexItem><RelativeTime date={installed.buildDate} prefix="built " size="inherit" /></FlexItem>}
            <FlexItem className="pf-v6-u-text-color-subtle">
              {updateAvailable === true && latest
                ? <>A newer {installed.tag} build exists{latest.buildDate ? <> (built <RelativeTime date={latest.buildDate} size="inherit" />)</> : null}.</>
                : updateAvailable === false ? <>This is the newest {installed.tag} build.</> : null}
            </FlexItem>
          </Flex>
        </DescriptionListDescription>
      </DescriptionListGroup>
    </DescriptionList>
  );
};

/** Syntax help for the build search, behind a help icon next to the field. */
const SearchHelp: React.FC = () => (
  <Popover
    headerContent="What you can search for"
    bodyContent={
      <Stack hasGutter>
        <StackItem><code>quay.io/rhoai/rhoai-fbc-fragment:&lt;tag&gt;@sha256:&lt;digest&gt;</code> shows that build.</StackItem>
        <StackItem>A commit SHA (7 to 40 characters) lists the builds with a component built from it.</StackItem>
        <StackItem><code>#123</code> or <code>PR 123</code> checks which builds contain that merged opendatahub-io/odh-dashboard PR.</StackItem>
        <StackItem>Commit and PR searches cover the installed build and the builds the filters list.</StackItem>
      </Stack>
    }
  >
    <Button variant="plain" aria-label="Search help" icon={<HelpIcon />} />
  </Popover>
);

export const BuildExplorerPage: React.FC = () => {
  const [searchParams] = useSearchParams();
  const { status, loading: statusLoading } = useClusterStatus();
  const [tags, setTags] = useState<NightlyTag[]>([]);
  const [loading, setLoading] = useState(true);
  const [datesLoading, setDatesLoading] = useState(false);
  const tagsRequestId = useRef(0);
  const [error, setError] = useState<ApiError | null>(null);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);

  const [expandedRows, setExpandedRows] = useState<Record<string, boolean>>({});
  const [versionFilter, setVersionFilter] = useState("all");
  const [typeFilter, setTypeFilter] = useState("all");
  const [versionSelectOpen, setVersionSelectOpen] = useState(false);
  const [typeSelectOpen, setTypeSelectOpen] = useState(false);

  const [searchText, setSearchText] = useState("");
  const [submitted, setSubmitted] = useState<{ kind: ReturnType<typeof classifySearch>["kind"]; value: string; nonce: number; builds: SearchBuild[] } | null>(null);
  const [compareTo, setCompareTo] = useState<BuildSide | null>(null);
  const compareRef = useRef<HTMLDivElement>(null);

  const installed = useMemo(() => installedBuild(status), [status]);
  const installedSide: BuildSide | null = installed ? { image: installed.image, label: INSTALLED_LABEL, buildDate: installed.buildDate } : null;

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
      // Show versions immediately; build dates are optional and arrive later.
      // Only merge dates for the exact image already shown.
      void getBuildExplorerTags(true)
        .then((enriched) => {
          if (requestId !== tagsRequestId.current) return;
          const dates = new Map((enriched.tags ?? []).map((tag) => [tag.image, tag.buildDate]));
          setTags((current) => current.map((tag) => ({ ...tag, buildDate: dates.get(tag.image) || tag.buildDate })));
        })
        .catch(() => { /* Build dates are optional; keep the version list usable. */ })
        .finally(() => {
          if (requestId === tagsRequestId.current) setDatesLoading(false);
        });
    } catch (e) {
      if (requestId === tagsRequestId.current) setError(toApiError(e, "Failed to fetch tags"));
    } finally {
      if (requestId === tagsRequestId.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchTags();
    const requests = tagsRequestId;
    return () => { requests.current++; };
  }, [fetchTags]);

  useEffect(() => {
    document.title = "Build Explorer — RHOAI Nightly Updater";
  }, []);

  const versionSeries = useMemo(() => {
    const majors = new Set<string>();
    for (const t of tags) {
      const parsed = parseTagVersion(t.tag);
      if (parsed) majors.add(parsed.series.split(".")[0]);
    }
    return Array.from(majors).sort((a, b) => parseInt(b, 10) - parseInt(a, 10));
  }, [tags]);

  const filteredTags = useMemo(() => tags
    .filter((t) => {
      const parsed = parseTagVersion(t.tag);
      if (!parsed) return false;
      if (versionFilter !== "all" && !parsed.series.startsWith(versionFilter + ".")) return false;
      if (typeFilter === "ea" && parsed.ea < 0) return false;
      if (typeFilter === "ga" && parsed.ea >= 0) return false;
      return true;
    })
    .sort((a, b) => {
      const pa = parseTagVersion(a.tag);
      const pb = parseTagVersion(b.tag);
      if (!pa || !pb) return 0;
      return compareTagsDesc(pa, pb);
    }), [tags, versionFilter, typeFilter]);

  const submitSearch = useCallback((raw: string) => {
    const { kind, value } = classifySearch(raw);
    if (kind === "empty") {
      setSubmitted(null);
      return;
    }
    // The commit search covers the installed build plus the builds listed now.
    const builds: SearchBuild[] = [];
    if (installed && !filteredTags.some((t) => imageDigest(t.image) === installed.digest)) {
      builds.push({ image: installed.image, label: `${INSTALLED_LABEL} (${shortBuildRef(installed.image)})` });
    }
    for (const t of filteredTags) builds.push({ image: t.image, label: t.tag });
    setSubmitted((prev) => ({ kind, value, nonce: (prev?.nonce ?? 0) + 1, builds }));
  }, [installed, filteredTags]);

  // ?image=... from the Status page's "View build contents" link.
  const urlImageApplied = useRef(false);
  useEffect(() => {
    if (urlImageApplied.current) return;
    const urlImage = searchParams.get("image");
    if (urlImage) {
      urlImageApplied.current = true;
      setSearchText(urlImage);
      setSubmitted({ kind: "image", value: urlImage, nonce: 1, builds: [] });
    }
  }, [searchParams]);

  const commitSearch = useCommitSearch(submitted?.kind === "commit" ? submitted.value : null, submitted?.builds ?? [], submitted?.nonce);
  const prSearch = usePRSearch(submitted?.kind === "pr" ? parseInt(submitted.value, 10) : null, submitted?.builds ?? [], submitted?.nonce);
  const retrySearch = useCallback(() => setSubmitted((prev) => (prev ? { ...prev, nonce: prev.nonce + 1 } : prev)), []);

  const openCompare = (to: BuildSide) => {
    setCompareTo(to);
    // Move focus to the comparison so keyboard and screen-reader users land on it.
    requestAnimationFrame(() => compareRef.current?.focus());
  };
  const clearSearch = () => {
    setSearchText("");
    setSubmitted(null);
  };
  /** True for the installed build, whatever its label in a result list. */
  const isInstalledImage = (image: string) => !!installed && imageDigest(image) === installed.digest;
  const updateAvailable = status?.nightly?.updateAvailable;
  const latest = status?.nightly?.latest;

  return (
    <>
      <PageHeader
        title="Build Explorer"
        description="Nightly builds on Quay: what each one contains, which builds include a commit or PR, and how they differ from the installed build."
        lastRefreshed={lastRefreshed}
        loading={loading}
        onRefresh={fetchTags}
        actions={installed && updateAvailable === true && latest ? (
          <Button variant="primary" onClick={() => openCompare({ image: latest.image, label: `Latest ${installed.tag}`, buildDate: latest.buildDate })}>
            Compare with the latest {installed.tag}
          </Button>
        ) : undefined}
        details={
          <InstalledBuildDetails
            installed={installed}
            statusLoaded={!!status || !statusLoading}
            latest={latest}
            updateAvailable={updateAvailable}
          />
        }
      />

      {error && tags.length > 0 && <LoadErrorAlert error={error} genericTitle="Could not load the nightly builds" onRetry={fetchTags} stale />}
      {error && tags.length === 0 && <PageErrorState error={error} title="Can't load the nightly builds" onRetry={fetchTags} />}
      {loading && !tags.length && !error && <PageLoading title="Loading the nightly builds from Quay" />}

      {compareTo && installedSide && (
        <PageSection>
          <div ref={compareRef} tabIndex={-1} aria-label="Build comparison" role="region">
            <BuildCompare from={installedSide} to={compareTo} onClose={() => setCompareTo(null)} />
          </div>
        </PageSection>
      )}

      {(tags.length > 0 || submitted) && (
        <PageSection isFilled>
          <Card>
            <CardHeader>
              <CardTitle><Title headingLevel="h2" size="lg">Nightly builds</Title></CardTitle>
            </CardHeader>
            <CardBody className="pf-v6-u-px-0">
              <Toolbar inset={{ default: "insetLg" }} clearAllFilters={() => { setVersionFilter("all"); setTypeFilter("all"); }}>
                <ToolbarContent alignItems="center">
                  <ToolbarItem className="pf-v6-u-w-100 pf-v6-u-w-33-on-lg">
                    <SearchInput
                      id="build-search"
                      placeholder="Image, commit SHA or PR #"
                      value={searchText}
                      onChange={(_e, val) => setSearchText(val)}
                      onSearch={(_e, val) => submitSearch(val)}
                      onClear={clearSearch}
                      aria-label="Find a build by image reference, commit SHA or PR number"
                    />
                  </ToolbarItem>
                  <ToolbarItem><SearchHelp /></ToolbarItem>
                  <ToolbarToggleGroup toggleIcon={<FilterIcon />} breakpoint="md">
                    <ToolbarGroup variant="filter-group">
                      <ToolbarItem>
                        <Select
                          isOpen={versionSelectOpen}
                          onOpenChange={setVersionSelectOpen}
                          onSelect={(_e, val) => { setVersionFilter(val as string); setVersionSelectOpen(false); }}
                          selected={versionFilter}
                          toggle={(toggleRef) => (
                            <MenuToggle ref={toggleRef} onClick={() => setVersionSelectOpen(!versionSelectOpen)} isExpanded={versionSelectOpen} aria-label="Version filter">
                              {versionFilter === "all" ? "All versions" : `v${versionFilter}.x`}
                            </MenuToggle>
                          )}
                        >
                          <SelectList aria-label="Version filter">
                            <SelectOption value="all">All versions</SelectOption>
                            {versionSeries.map((v) => <SelectOption key={v} value={v}>v{v}.x</SelectOption>)}
                          </SelectList>
                        </Select>
                      </ToolbarItem>
                      <ToolbarItem>
                        <Select
                          isOpen={typeSelectOpen}
                          onOpenChange={setTypeSelectOpen}
                          onSelect={(_e, val) => { setTypeFilter(val as string); setTypeSelectOpen(false); }}
                          selected={typeFilter}
                          toggle={(toggleRef) => (
                            <MenuToggle ref={toggleRef} onClick={() => setTypeSelectOpen(!typeSelectOpen)} isExpanded={typeSelectOpen} aria-label="Type filter">
                              {TYPE_LABELS[typeFilter]}
                            </MenuToggle>
                          )}
                        >
                          <SelectList aria-label="Type filter">
                            {Object.entries(TYPE_LABELS).map(([id, label]) => <SelectOption key={id} value={id}>{label}</SelectOption>)}
                          </SelectList>
                        </Select>
                      </ToolbarItem>
                    </ToolbarGroup>
                  </ToolbarToggleGroup>
                  <ToolbarItem variant="pagination" align={{ default: "alignEnd" }}>
                    <span className="pf-v6-u-text-color-subtle pf-v6-u-font-size-sm">
                      {submitted ? "Search results" : <>{filteredTags.length} of {tags.length} tags</>}
                    </span>
                  </ToolbarItem>
                </ToolbarContent>
              </Toolbar>

              {submitted ? (
                <Stack hasGutter className="pf-v6-u-px-lg">
                  <StackItem>
                    <SearchResults
                      submitted={submitted}
                      commitSearch={commitSearch}
                      prSearch={prSearch}
                      canCompare={!!installedSide}
                      isInstalledImage={isInstalledImage}
                      onCompare={openCompare}
                      onRetry={retrySearch}
                    />
                  </StackItem>
                  <StackItem>
                    <Button variant="link" onClick={clearSearch}>Clear the search and show all builds</Button>
                  </StackItem>
                </Stack>
              ) : (
                <Table aria-label="Nightly builds" variant="compact" gridBreakPoint="grid-md" borders={false}>
                  <Thead>
                    <Tr>
                      <Th screenReaderText="Expand row" />
                      <Th width={30}>Tag</Th>
                      <Th modifier="fitContent">Type</Th>
                      <Th modifier="nowrap">Built</Th>
                      <Th>Digest</Th>
                      <Th screenReaderText="Actions" />
                    </Tr>
                  </Thead>
                  {filteredTags.map((tag, rowIndex) => {
                    const key = tag.tag;
                    const isExpanded = !!expandedRows[key];
                    const parsed = parseTagVersion(tag.tag);
                    const isEA = !!parsed && parsed.ea >= 0;
                    const isInstalled = isInstalledImage(tag.image);
                    const isNewerThanInstalled = !!installed && !isInstalled && updateAvailable === true && installed.tag === tag.tag;
                    const toggle = () => setExpandedRows((prev) => ({ ...prev, [key]: !prev[key] }));
                    const actions: IAction[] = [
                      ...(installedSide && !isInstalled
                        ? [{ title: "Compare with installed", onClick: () => openCompare({ image: tag.image, label: tag.tag, buildDate: tag.buildDate }) }]
                        : []),
                      { title: isExpanded ? "Hide contents" : "View contents", onClick: toggle },
                      { title: "Copy image reference", onClick: () => copy(tag.image) },
                    ];
                    return (
                      <Tbody key={key} isExpanded={isExpanded}>
                        <Tr isStriped={rowIndex % 2 === 1}>
                          <Td expand={{ rowIndex, isExpanded, onToggle: toggle }} />
                          <Td dataLabel="Tag" id={`simple-node${rowIndex}`}>
                            <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                              <strong>{tag.tag}</strong>
                              {isInstalled && <span><TagLabel color="blue">Installed</TagLabel></span>}
                              {isNewerThanInstalled && <span><TagLabel color="teal">Newer than installed</TagLabel></span>}
                            </Flex>
                          </Td>
                          <Td dataLabel="Type" modifier="fitContent">
                            <span>
                              <TagLabel color={isEA ? "purple" : "grey"}>
                                {isEA ? (tag.tag.endsWith("-ea") ? "EA" : `EA ${parsed?.ea}`) : "GA"}
                              </TagLabel>
                            </span>
                          </Td>
                          <Td dataLabel="Built" modifier="nowrap">
                            {datesLoading && !tag.buildDate ? (
                              <Skeleton width="4rem" screenreaderText="Loading build date" />
                            ) : tag.buildDate ? (
                              <RelativeTime date={tag.buildDate} />
                            ) : <NotRecorded what="Build date not recorded" />}
                          </Td>
                          <Td dataLabel="Digest"><ImageRef image={tag.image} display={imageDigest(tag.image).replace("sha256:", "").slice(0, 12) || tag.image} /></Td>
                          <Td isActionCell><RowActions items={actions} rowName={tag.tag} /></Td>
                        </Tr>
                        <Tr isExpanded={isExpanded}>
                          <Td colSpan={6}>
                            <ExpandableRowContent>
                              {isExpanded && <BuildContents image={tag.image} title={tag.tag} />}
                            </ExpandableRowContent>
                          </Td>
                        </Tr>
                      </Tbody>
                    );
                  })}
                </Table>
              )}
            </CardBody>
          </Card>
        </PageSection>
      )}
    </>
  );
};

const SearchResults: React.FC<{
  submitted: { kind: string; value: string; nonce: number; builds: SearchBuild[] };
  commitSearch: ReturnType<typeof useCommitSearch>;
  prSearch: ReturnType<typeof usePRSearch>;
  canCompare: boolean;
  isInstalledImage: (image: string) => boolean;
  onCompare: (to: BuildSide) => void;
  onRetry: () => void;
}> = ({ submitted, commitSearch, prSearch, canCompare, isInstalledImage, onCompare, onRetry }) => {
  if (submitted.kind === "image") {
    return <BuildContents key={`${submitted.value}|${submitted.nonce}`} image={submitted.value} title="the searched build" />;
  }
  if (submitted.kind === "pr") {
    return (
      <PRSearchResults
        key={submitted.nonce}
        pr={submitted.value}
        search={prSearch}
        plannedTotal={submitted.builds.length}
        canCompare={canCompare}
        isInstalledImage={isInstalledImage}
        onCompare={onCompare}
        onRetry={onRetry}
      />
    );
  }
  if (submitted.kind === "invalid") {
    return (
      <Alert component="p" variant="warning" isInline isLiveRegion title="Not a build reference, commit SHA or PR number">
        Enter an image reference such as quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:..., a commit SHA of 7 to 40 hex characters, or a PR number such as #123.
      </Alert>
    );
  }
  const short = submitted.value.slice(0, 12);
  const { checked, matches, failures } = commitSearch;
  // Before the first progress update the hook still holds the previous (idle) state.
  const total = commitSearch.total || submitted.builds.length;
  const running = commitSearch.running || commitSearch.total === 0;
  return (
    <Stack hasGutter>
      <StackItem>
        <Content component="p" aria-live="polite">
          {running ? <><Spinner size="sm" aria-label="Searching builds" />{" "}Checked {checked} of {total} builds for commit <code>{short}</code>...</>
            : <>Checked {total} builds: {matches.length === 0 ? "no build has a component built from" : `${matches.length} ${matches.length === 1 ? "build has" : "builds have"} a component built from`} commit <code>{short}</code>. Later builds also contain the commit but are not listed.</>}
        </Content>
      </StackItem>
      {matches.length > 0 && (
        <StackItem>
          <Table aria-label={`Builds with a component built from ${short}`} variant="compact" borders={false}>
            <Thead><Tr><Th width={25}>Build</Th><Th>Components built from this commit</Th><Th screenReaderText="Actions" /></Tr></Thead>
            <Tbody>
              {matches.map((m, rowIndex) => (
                <Tr key={m.build.image} isStriped={rowIndex % 2 === 1}>
                  <Td dataLabel="Build"><strong>{m.build.label}</strong></Td>
                  <Td dataLabel="Components">{m.images.map((i) => i.name).join(", ")}</Td>
                  <Td isActionCell>
                    <RowActions
                      rowName={m.build.label}
                      items={[
                        ...(canCompare && !isInstalledImage(m.build.image) ? [{ title: "Compare with installed", onClick: () => onCompare({ image: m.build.image, label: m.build.label }) }] : []),
                        { title: "Copy image reference", onClick: () => copy(m.build.image) },
                      ]}
                    />
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        </StackItem>
      )}
      {failures.length > 0 && !running && (
        <StackItem>
          <Alert component="p" variant="warning" isInline isPlain title={`${failures.length} builds could not be read`}>
            {failures.map((f) => `${f.build.label}: ${f.message}`).join("; ")}
          </Alert>
        </StackItem>
      )}
    </Stack>
  );
};
