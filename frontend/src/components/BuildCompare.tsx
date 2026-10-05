import React, { useMemo, useState } from "react";
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  ExpandableSection,
  Flex,
  FlexItem,
  Label,
  List,
  ListItem,
  SearchInput,
  Skeleton,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { useFbcContent } from "../hooks/useFbcContent";
import { formatRelativeTime } from "../utils";
import { compareBuilds, compareURL, imageDigest, shortBuildRef, type RepoChange } from "./buildDiff";

export interface BuildSide {
  image: string;
  /** "Installed", "rhoai-3.6", ... */
  label: string;
  buildDate?: string;
}

const MAX_NAMES = 3;

const ImageNames: React.FC<{ change: RepoChange }> = ({ change }) => {
  const [all, setAll] = useState(false);
  const names = change.images.map((i) => i.name);
  const shown = all ? names : names.slice(0, MAX_NAMES);
  return (
    <Content component="small" style={{ overflowWrap: "anywhere" }}>
      {shown.join(", ")}
      {names.length > MAX_NAMES && (
        <>
          {" "}
          <Button variant="link" isInline size="sm" onClick={() => setAll((v) => !v)} aria-expanded={all}>
            {all ? "Show fewer" : `and ${names.length - MAX_NAMES} more`}
          </Button>
        </>
      )}
    </Content>
  );
};

const Sha: React.FC<{ sha?: string }> = ({ sha }) => (sha ? <code title={sha}>{sha.slice(0, 7)}</code> : <span>-</span>);

const SideText: React.FC<{ side: BuildSide }> = ({ side }) => (
  <>
    <strong>{side.label}</strong> <code>{shortBuildRef(side.image)}</code>
    {side.buildDate && <> (built <time dateTime={side.buildDate} title={new Date(side.buildDate).toLocaleString()}>{formatRelativeTime(side.buildDate)}</time>)</>}
  </>
);

/**
 * "Compare with installed" (A08-5): the repositories whose commits differ
 * between two builds, each with one GitHub compare link, plus images that
 * were added, removed or only rebuilt. Uses the FBC content and image labels
 * the API already returns; the git labels arrive after the catalog.
 */
export const BuildCompare: React.FC<{ from: BuildSide; to: BuildSide; onClose: () => void }> = ({ from, to, onClose }) => {
  const a = useFbcContent(from.image);
  const b = useFbcContent(to.image);
  const [filter, setFilter] = useState("");
  const same = imageDigest(from.image) !== "" && imageDigest(from.image) === imageDigest(to.image);
  const ready = a.labelsDone && b.labelsDone && !!a.data && !!b.data;
  const result = useMemo(
    () => (ready ? compareBuilds(a.data?.relatedImages ?? [], b.data?.relatedImages ?? []) : null),
    [ready, a.data, b.data],
  );
  const needle = filter.trim().toLowerCase();
  const repos = (result?.repos ?? []).filter((r) => !needle || r.repo.toLowerCase().includes(needle) || r.images.some((i) => i.name.toLowerCase().includes(needle)));
  const withCommits = new Set(result?.repos.filter((r) => r.kind === "changed").map((r) => r.gitURL)).size;
  const rebuilt = result?.repos.filter((r) => r.kind === "rebuilt").reduce((n, r) => n + r.images.length, 0) ?? 0;
  const error = a.error ?? b.error;

  return (
    <Card>
      <CardHeader actions={{ actions: <Button variant="secondary" onClick={onClose}>Close comparison</Button>, hasNoOffset: true }}>
        <CardTitle component="h2">Compare builds</CardTitle>
        <Content component="small">
          <SideText side={from} /> &rarr; <SideText side={{ ...to, label: `Selected: ${to.label}` }} />
        </Content>
      </CardHeader>
      <CardBody>
        {same ? (
          <Alert component="p" variant="info" isInline title="Both are the same build (same digest)." />
        ) : error ? (
          <Alert component="p" variant="danger" isInline isLiveRegion title="Could not load a build to compare">{error.message}</Alert>
        ) : !result ? (
          <div aria-busy="true">
            <Content component="small" aria-live="polite">
              {a.loading || b.loading ? "Reading both catalogs..." : "Resolving git commits of both builds..."}
            </Content>
            <Skeleton height="1.5rem" screenreaderText="Comparing builds" />
            <Skeleton height="1.5rem" />
          </div>
        ) : (
          <Stack hasGutter>
            <StackItem>
              <Flex gap={{ default: "gapSm" }} flexWrap={{ default: "wrap" }} aria-live="polite">
                <FlexItem><Label color="blue">{withCommits} repositories with new commits</Label></FlexItem>
                <FlexItem><Label color="grey">{result.changedImages} images changed</Label></FlexItem>
                {rebuilt > 0 && <FlexItem><Label color="grey">{rebuilt} rebuilt from the same commit</Label></FlexItem>}
                {result.added.length > 0 && <FlexItem><Label color="green">{result.added.length} added</Label></FlexItem>}
                {result.removed.length > 0 && <FlexItem><Label color="orange">{result.removed.length} removed</Label></FlexItem>}
                <FlexItem><Label color="grey" variant="outline">{result.unchangedImages} unchanged</Label></FlexItem>
              </Flex>
              {(a.data?.bundleName || b.data?.bundleName) && (
                <Content component="small">Operator bundle: {a.data?.bundleName || "unknown"} &rarr; {b.data?.bundleName || "unknown"}</Content>
              )}
            </StackItem>
            {result.repos.length > 0 && (
              <StackItem>
                <SearchInput aria-label="Filter compared repositories" placeholder="Filter by repository or image" value={filter} onChange={(_e, v) => setFilter(v)} onClear={() => setFilter("")} style={{ maxWidth: "24rem" }} />
              </StackItem>
            )}
            <StackItem>
              {result.repos.length === 0 ? (
                <Content component="p">No image differs between the two builds.</Content>
              ) : (
                <Table aria-label="Repositories that differ" variant="compact">
                  <Thead>
                    <Tr>
                      <Th>Repository</Th>
                      <Th modifier="nowrap">Installed</Th>
                      <Th modifier="nowrap">Selected</Th>
                      <Th>Images</Th>
                      <Th screenReaderText="GitHub compare" />
                    </Tr>
                  </Thead>
                  <Tbody>
                    {repos.map((r) => {
                      const url = compareURL(r);
                      return (
                        <Tr key={`${r.gitURL}|${r.fromCommit}|${r.toCommit}|${r.kind}`}>
                          <Td dataLabel="Repository">
                            <span style={{ overflowWrap: "anywhere" }}>{r.repo}</span>
                            {r.kind === "rebuilt" && <>{" "}<Label isCompact color="grey">Rebuilt, same commit</Label></>}
                            {r.kind === "unknown" && <>{" "}<Label isCompact color="grey">No commit labels</Label></>}
                          </Td>
                          <Td dataLabel="Installed"><Sha sha={r.fromCommit} /></Td>
                          <Td dataLabel="Selected"><Sha sha={r.toCommit} /></Td>
                          <Td dataLabel="Images"><ImageNames change={r} /></Td>
                          <Td dataLabel="GitHub compare">
                            {url && (
                              <Button variant="link" isInline component="a" href={url} target="_blank" rel="noopener noreferrer" icon={<ExternalLinkAltIcon />} iconPosition="end" size="sm" aria-label={`Compare ${r.repo} commits on GitHub`}>
                                Compare
                              </Button>
                            )}
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
              )}
            </StackItem>
            {(result.added.length > 0 || result.removed.length > 0) && (
              <StackItem>
                <ExpandableSection toggleText={`Added and removed images (${result.added.length + result.removed.length})`}>
                  <List>
                    {result.added.map((i) => <ListItem key={`+${i.name}`}><Label isCompact color="green">Added</Label> {i.name}</ListItem>)}
                    {result.removed.map((i) => <ListItem key={`-${i.name}`}><Label isCompact color="orange">Removed</Label> {i.name}</ListItem>)}
                  </List>
                </ExpandableSection>
              </StackItem>
            )}
          </Stack>
        )}
      </CardBody>
    </Card>
  );
};
