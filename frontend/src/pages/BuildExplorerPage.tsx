import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  Alert,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  Content,
  Flex,
  FlexItem,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Label,
  MenuToggle,
  PageSection,
  SearchInput,
  Select,
  SelectList,
  SelectOption,
  Skeleton,
  Spinner,
  Stack,
  StackItem,
  Title,
  ToggleGroup,
  ToggleGroupItem,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import { Table, Thead, Tbody, Tr, Th, Td, ExpandableRowContent } from "@patternfly/react-table";
import type { NightlyTag } from "../types";
import { ApiError, getBuildExplorerTags, toApiError } from "../services/api";
import { formatRelativeTime } from "../utils";
import { PageHeader } from "../components/PageHeader";
import { LoadErrorAlert } from "../components/LoadErrorAlert";
import { ImageRef } from "../components/ImageRef";
import { BuildContents } from "../components/BuildContents";
import { BuildCompare, type BuildSide } from "../components/BuildCompare";
import { classifySearch, imageDigest, installedBuild, shortBuildRef, type InstalledBuild } from "../components/buildDiff";
import { useClusterStatus } from "../state/AppState";
import { useCommitSearch, type SearchBuild } from "../hooks/useCommitSearch";

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

/** The installed build and whether its stream has a newer build (status.nightly, B5). */
const InstalledBuildCard: React.FC<{
  installed: InstalledBuild | null;
  statusLoaded: boolean;
  latest?: { image: string; buildDate?: string };
  updateAvailable?: boolean;
  onCompare: (to: BuildSide) => void;
}> = ({ installed, statusLoaded, latest, updateAvailable, onCompare }) => {
  if (!statusLoaded) {
    return <Card isCompact><CardBody><Skeleton width="40%" screenreaderText="Loading the installed build" /></CardBody></Card>;
  }
  if (!installed) {
    return (
      <Alert component="p" variant="info" isInline title="The installed build is unknown">
        RHOAI on this cluster was not installed from a nightly build, so there is no installed build to mark or compare with.
      </Alert>
    );
  }
  return (
    <Card isCompact>
      <CardBody>
        <Flex justifyContent={{ default: "justifyContentSpaceBetween" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
          <FlexItem style={{ minWidth: 0 }}>
            <Title headingLevel="h2" size="md">
              Installed build: <code>{shortBuildRef(installed.image)}</code>
            </Title>
            <Content component="small">
              {installed.buildDate && <>Built <time dateTime={installed.buildDate} title={new Date(installed.buildDate).toLocaleString()}>{formatRelativeTime(installed.buildDate)}</time>. </>}
              {updateAvailable === true && latest
                ? <>A newer {installed.tag} build exists{latest.buildDate ? <> (built {formatRelativeTime(latest.buildDate)})</> : null}.</>
                : updateAvailable === false ? <>This is the newest {installed.tag} build.</> : null}
            </Content>
            <div style={{ maxWidth: "36rem" }}><ImageRef image={installed.image} what="installed image reference" /></div>
          </FlexItem>
          {updateAvailable === true && latest && (
            <FlexItem>
              <Button variant="primary" onClick={() => onCompare({ image: latest.image, label: `Latest ${installed.tag}`, buildDate: latest.buildDate })}>
                Compare with the latest {installed.tag}
              </Button>
            </FlexItem>
          )}
        </Flex>
      </CardBody>
    </Card>
  );
};

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

  // ?image=... from the Dashboard's "Preview contents" link.
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

  const openCompare = (to: BuildSide) => {
    setCompareTo(to);
    // Move focus to the comparison so keyboard and screen-reader users land on it.
    requestAnimationFrame(() => compareRef.current?.focus());
  };

  return (
    <>
      <PageHeader title="Build Explorer" lastRefreshed={lastRefreshed} loading={loading} onRefresh={fetchTags} />

      {error && <LoadErrorAlert error={error} genericTitle="Could not load the nightly builds" onRetry={fetchTags} stale={tags.length > 0} />}

      <PageSection>
        <Stack hasGutter>
          <StackItem>
            <InstalledBuildCard
              installed={installed}
              statusLoaded={!!status || !statusLoading}
              latest={status?.nightly?.latest}
              updateAvailable={status?.nightly?.updateAvailable}
              onCompare={openCompare}
            />
          </StackItem>

          {compareTo && installedSide && (
            <StackItem>
              <div ref={compareRef} tabIndex={-1} aria-label="Build comparison" role="region">
                <BuildCompare from={installedSide} to={compareTo} onClose={() => setCompareTo(null)} />
              </div>
            </StackItem>
          )}

          <StackItem>
            <Card isCompact>
              <CardBody>
                <FormGroup label="Find a build" fieldId="build-search">
                  <SearchInput
                    id="build-search"
                    placeholder="Image reference, commit SHA, or PR number"
                    value={searchText}
                    onChange={(_e, val) => setSearchText(val)}
                    onSearch={(_e, val) => submitSearch(val)}
                    onClear={() => { setSearchText(""); setSubmitted(null); }}
                    aria-label="Find a build by image reference, commit SHA or PR number"
                    aria-describedby="build-search-help"
                  />
                  <FormHelperText>
                    <HelperText id="build-search-help">
                      <HelperTextItem>
                        <code>quay.io/rhoai/rhoai-fbc-fragment:&lt;tag&gt;@sha256:&lt;digest&gt;</code> shows that build. A commit SHA (7-40 characters) lists the builds with a component built from it.
                      </HelperTextItem>
                    </HelperText>
                  </FormHelperText>
                </FormGroup>
                <SearchResults
                  submitted={submitted}
                  commitSearch={commitSearch}
                  canCompare={!!installedSide}
                  onCompare={openCompare}
                />
              </CardBody>
            </Card>
          </StackItem>
        </Stack>
      </PageSection>

      {loading && !tags.length && !error && (
        <PageSection>
          <Bullseye>
            <Flex direction={{ default: "column" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapMd" }}>
              <FlexItem><Spinner size="xl" aria-label="Loading tags" /></FlexItem>
              <FlexItem><Content component="p">Fetching nightly build tags from Quay...</Content></FlexItem>
            </Flex>
          </Bullseye>
        </PageSection>
      )}

      {tags.length > 0 && (
        <PageSection>
          <Card>
            <CardTitle>
              <Title headingLevel="h2" size="lg">Latest builds per tag</Title>
            </CardTitle>
            <CardBody>
              <Toolbar>
                <ToolbarContent>
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
                        {versionSeries.map((s) => <SelectOption key={s} value={s}>v{s}.x</SelectOption>)}
                      </SelectList>
                    </Select>
                  </ToolbarItem>
                  <ToolbarItem>
                    <ToggleGroup aria-label="Type filter" isCompact>
                      <ToggleGroupItem text="All" isSelected={typeFilter === "all"} onChange={() => setTypeFilter("all")} />
                      <ToggleGroupItem text="EA" isSelected={typeFilter === "ea"} onChange={() => setTypeFilter("ea")} />
                      <ToggleGroupItem text="GA" isSelected={typeFilter === "ga"} onChange={() => setTypeFilter("ga")} />
                    </ToggleGroup>
                  </ToolbarItem>
                  <ToolbarItem alignSelf="center">
                    <Content component="small">{filteredTags.length} of {tags.length} tags</Content>
                  </ToolbarItem>
                </ToolbarContent>
              </Toolbar>
              <Table aria-label="Nightly builds" variant="compact">
                <Thead>
                  <Tr>
                    <Th screenReaderText="Expand row" />
                    <Th>Tag</Th>
                    <Th>Type</Th>
                    <Th>Built</Th>
                    <Th>Digest</Th>
                    <Th screenReaderText="Actions" />
                  </Tr>
                </Thead>
                {filteredTags.map((tag, rowIndex) => {
                  const key = tag.tag;
                  const isExpanded = !!expandedRows[key];
                  const parsed = parseTagVersion(tag.tag);
                  const isEA = !!parsed && parsed.ea >= 0;
                  const isInstalled = !!installed && imageDigest(tag.image) === installed.digest;
                  const isNewerThanInstalled = !!installed && !isInstalled && status?.nightly?.updateAvailable === true && installed.tag === tag.tag;
                  return (
                    <Tbody key={key} isExpanded={isExpanded}>
                      <Tr>
                        <Td expand={{ rowIndex, isExpanded, onToggle: () => setExpandedRows((prev) => ({ ...prev, [key]: !prev[key] })) }} />
                        <Td dataLabel="Tag" id={`simple-node${rowIndex}`} modifier="nowrap">
                          <strong>{tag.tag}</strong>
                          {isInstalled && <>{" "}<Label isCompact color="green">Installed</Label></>}
                          {isNewerThanInstalled && <>{" "}<Label isCompact color="blue">Newer than installed</Label></>}
                        </Td>
                        <Td dataLabel="Type">
                          <Label isCompact color={isEA ? "orange" : "grey"}>
                            {isEA ? (tag.tag.endsWith("-ea") ? "EA" : `EA ${parsed?.ea}`) : "GA"}
                          </Label>
                        </Td>
                        <Td dataLabel="Built">
                          {datesLoading && !tag.buildDate ? (
                            <Skeleton width="4rem" screenreaderText="Loading build date" />
                          ) : tag.buildDate ? (
                            <Content component="small"><time dateTime={tag.buildDate} title={new Date(tag.buildDate).toLocaleString()}>{formatRelativeTime(tag.buildDate)}</time></Content>
                          ) : <Content component="small">-</Content>}
                        </Td>
                        <Td dataLabel="Digest"><ImageRef image={tag.image} display={imageDigest(tag.image).slice(0, 19) || tag.image} /></Td>
                        <Td dataLabel="Actions" modifier="nowrap">
                          {installedSide && !isInstalled && (
                            <Button variant="secondary" size="sm" onClick={() => openCompare({ image: tag.image, label: tag.tag, buildDate: tag.buildDate })} aria-label={`Compare ${tag.tag} with the installed build`}>
                              Compare with installed
                            </Button>
                          )}
                        </Td>
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
            </CardBody>
          </Card>
        </PageSection>
      )}
    </>
  );
};

const SearchResults: React.FC<{
  submitted: { kind: string; value: string; nonce: number; builds: SearchBuild[] } | null;
  commitSearch: ReturnType<typeof useCommitSearch>;
  canCompare: boolean;
  onCompare: (to: BuildSide) => void;
}> = ({ submitted, commitSearch, canCompare, onCompare }) => {
  if (!submitted) return null;
  if (submitted.kind === "image") {
    return (
      <div style={{ marginTop: "var(--pf-t--global--spacer--md)" }}>
        <BuildContents key={`${submitted.value}|${submitted.nonce}`} image={submitted.value} title="the searched build" />
      </div>
    );
  }
  if (submitted.kind === "pr") {
    return (
      <Alert component="p" variant="info" isInline isLiveRegion title={`Search by PR number (#${submitted.value}) is not available`} style={{ marginTop: "var(--pf-t--global--spacer--md)" }}>
        The build data records the commit each component was built from, not the pull requests it contains, and this page
        cannot ask GitHub. Paste the PR&apos;s merge commit SHA instead (shown at the end of the PR page): the builds with a
        component built from exactly that commit are listed. Later builds of the same stream also contain it.
      </Alert>
    );
  }
  if (submitted.kind === "invalid") {
    return (
      <Alert component="p" variant="warning" isInline isLiveRegion title="Not a build reference, commit SHA or PR number" style={{ marginTop: "var(--pf-t--global--spacer--md)" }}>
        Enter an image reference such as quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:..., a commit SHA of 7 to 40 hex characters, or a PR number.
      </Alert>
    );
  }
  const short = submitted.value.slice(0, 12);
  const { checked, matches, failures } = commitSearch;
  // Before the first progress update the hook still holds the previous (idle) state.
  const total = commitSearch.total || submitted.builds.length;
  const running = commitSearch.running || commitSearch.total === 0;
  return (
    <div style={{ marginTop: "var(--pf-t--global--spacer--md)" }}>
      <Content component="p" aria-live="polite">
        {running ? <><Spinner size="sm" aria-label="Searching builds" />{" "}Checked {checked} of {total} builds for commit <code>{short}</code>...</>
          : <>Checked {total} builds: {matches.length === 0 ? "no build has a component built from" : `${matches.length} ${matches.length === 1 ? "build has" : "builds have"} a component built from`} commit <code>{short}</code>.</>}
      </Content>
      <Content component="small">
        The search covers the installed build and the builds listed below (use the filters to narrow it). It matches the exact
        commit a component was built from; later builds also contain that commit but are not listed.
      </Content>
      {matches.length > 0 && (
        <Table aria-label={`Builds with a component built from ${short}`} variant="compact">
          <Thead><Tr><Th>Build</Th><Th>Components built from this commit</Th><Th screenReaderText="Actions" /></Tr></Thead>
          <Tbody>
            {matches.map((m) => (
              <Tr key={m.build.image}>
                <Td dataLabel="Build"><strong>{m.build.label}</strong></Td>
                <Td dataLabel="Components">{m.images.map((i) => i.name).join(", ")}</Td>
                <Td dataLabel="Actions" modifier="nowrap">
                  {canCompare && !m.build.label.startsWith(INSTALLED_LABEL) && (
                    <Button variant="secondary" size="sm" onClick={() => onCompare({ image: m.build.image, label: m.build.label })}>Compare with installed</Button>
                  )}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
      {failures.length > 0 && !running && (
        <Alert component="p" variant="warning" isInline isPlain title={`${failures.length} builds could not be read`}>
          {failures.map((f) => `${f.build.label}: ${f.message}`).join("; ")}
        </Alert>
      )}
    </div>
  );
};
