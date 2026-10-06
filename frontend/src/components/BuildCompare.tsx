import React, { useMemo, useState } from "react";
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
  ExpandableSection,
  List,
  ListItem,
  SearchInput,
  Skeleton,
  Stack,
  StackItem,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import TimesIcon from "@patternfly/react-icons/dist/esm/icons/times-icon";
import { useFbcContent } from "../hooks/useFbcContent";
import { NotRecorded, RelativeTime } from "./RelativeTime";
import { TagLabel } from "./StatusLabel";
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
    <span className="pf-v6-u-text-break-word">
      {shown.join(", ")}
      {names.length > MAX_NAMES && (
        <>
          {" "}
          <Button variant="link" isInline onClick={() => setAll((v) => !v)} aria-expanded={all}>
            {all ? "show fewer" : `and ${names.length - MAX_NAMES} more`}
          </Button>
        </>
      )}
    </span>
  );
};

const Sha: React.FC<{ sha?: string }> = ({ sha }) => (sha ? <code title={sha}>{sha.slice(0, 7)}</code> : <NotRecorded what="No commit label" />);

const SideText: React.FC<{ side: BuildSide }> = ({ side }) => (
  <>
    <strong>{side.label}</strong> <code>{shortBuildRef(side.image)}</code>
    {side.buildDate && <> (built <RelativeTime date={side.buildDate} size="inherit" />)</>}
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
      <CardHeader actions={{ actions: <Button variant="plain" icon={<TimesIcon />} aria-label="Close comparison" onClick={onClose} />, hasNoOffset: true }}>
        <CardTitle><Title headingLevel="h2" size="lg">Compare builds</Title></CardTitle>
      </CardHeader>
      <CardBody>
        <DescriptionList isCompact isHorizontal horizontalTermWidthModifier={{ default: "10ch" }} className="pf-v6-u-mb-md">
          <DescriptionListGroup>
            <DescriptionListTerm>From</DescriptionListTerm>
            <DescriptionListDescription><SideText side={from} /></DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>To</DescriptionListTerm>
            <DescriptionListDescription><SideText side={to} /></DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
        {same ? (
          <Alert component="p" variant="info" isInline title="Both are the same build (same digest)." />
        ) : error ? (
          <Alert component="p" variant="danger" isInline isLiveRegion title="Could not load a build to compare">{error.message}</Alert>
        ) : !result ? (
          <Stack hasGutter aria-busy="true">
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle" aria-live="polite">
                {a.loading || b.loading ? "Reading both catalogs..." : "Resolving git commits of both builds..."}
              </Content>
            </StackItem>
            <StackItem><Skeleton height="1.5rem" screenreaderText="Comparing builds" /></StackItem>
            <StackItem><Skeleton height="1.5rem" /></StackItem>
          </Stack>
        ) : (
          <Stack hasGutter>
            <StackItem>
              <Content component="p" aria-live="polite">
                {[
                  `${withCommits} ${withCommits === 1 ? "repository" : "repositories"} with new commits`,
                  `${result.changedImages} ${result.changedImages === 1 ? "image" : "images"} changed`,
                  rebuilt > 0 ? `${rebuilt} rebuilt from the same commit` : "",
                  result.added.length > 0 ? `${result.added.length} added` : "",
                  result.removed.length > 0 ? `${result.removed.length} removed` : "",
                  `${result.unchangedImages} unchanged`,
                ].filter(Boolean).join(" · ")}
              </Content>
              {(a.data?.bundleName || b.data?.bundleName) && (
                <Content component="p" className="pf-v6-u-text-color-subtle">Operator bundle: {a.data?.bundleName || "unknown"} &rarr; {b.data?.bundleName || "unknown"}</Content>
              )}
            </StackItem>
            {result.repos.length === 0 && (
              <StackItem><Content component="p">No image differs between the two builds.</Content></StackItem>
            )}
          </Stack>
        )}
      </CardBody>
      {!same && !error && result && result.repos.length > 0 && (
        <CardBody className="pf-v6-u-px-0">
                <Toolbar inset={{ default: "insetLg" }}>
                  <ToolbarContent>
                    <ToolbarItem>
                      <SearchInput aria-label="Filter compared repositories" placeholder="Filter by repository or image" value={filter} onChange={(_e, v) => setFilter(v)} onClear={() => setFilter("")} />
                    </ToolbarItem>
                  </ToolbarContent>
                </Toolbar>
                <Table aria-label="Repositories that differ" variant="compact" borders={false}>
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
                    {repos.map((r, rowIndex) => {
                      const url = compareURL(r);
                      return (
                        <Tr key={`${r.gitURL}|${r.fromCommit}|${r.toCommit}|${r.kind}`} isStriped={rowIndex % 2 === 1}>
                          <Td dataLabel="Repository">
                            <span className="pf-v6-u-text-break-word">{r.repo}</span>
                            {r.kind === "rebuilt" && <>{" "}<TagLabel>Rebuilt, same commit</TagLabel></>}
                            {r.kind === "unknown" && <>{" "}<TagLabel>No commit labels</TagLabel></>}
                          </Td>
                          <Td dataLabel="Installed"><Sha sha={r.fromCommit} /></Td>
                          <Td dataLabel="Selected"><Sha sha={r.toCommit} /></Td>
                          <Td dataLabel="Images"><ImageNames change={r} /></Td>
                          <Td dataLabel="GitHub compare">
                            {url && (
                              <a href={url} target="_blank" rel="noopener noreferrer" aria-label={`Compare ${r.repo} commits on GitHub`}>
                                Compare <ExternalLinkAltIcon />
                              </a>
                            )}
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
        </CardBody>
      )}
      {!same && !error && result && (result.added.length > 0 || result.removed.length > 0) && (
        <CardBody>
                <ExpandableSection toggleText={`Added and removed images (${result.added.length + result.removed.length})`}>
                  <List>
                    {result.added.map((i) => <ListItem key={`+${i.name}`}><TagLabel color="teal">Added</TagLabel> {i.name}</ListItem>)}
                    {result.removed.map((i) => <ListItem key={`-${i.name}`}><TagLabel>Removed</TagLabel> {i.name}</ListItem>)}
                  </List>
                </ExpandableSection>
        </CardBody>
      )}
    </Card>
  );
};
