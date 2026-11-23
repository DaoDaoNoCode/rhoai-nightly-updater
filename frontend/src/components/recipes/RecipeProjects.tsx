import React from "react";
import { Alert, Content, Stack, StackItem, Title } from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, Tr } from "@patternfly/react-table";
import type { RecipeOwnedProject } from "../../types";
import { RelativeTime } from "../RelativeTime";
import { TooltipButton } from "../TooltipButton";

/**
 * Namespaces the cluster still holds after recipe runs (GET
 * /api/recipes/run projects: persisted UID, the tool’s visible label, and
 * recipe annotation while delete is not underway), including this run and
 * previous ones; each may be removed separately, with Clean up enforcing
 * safeguards. When the listing call fails, the error is shown and no table is
 * rendered.
 */
export const RecipeProjects: React.FC<{
  projects: RecipeOwnedProject[];
  error?: string;
  /** Human-readable reason an operation cannot begin yet; null when unblocked. */
  blocker: string | null;
  /** Delete this project: the one the row shows (its UID too). */
  onDelete: (project: RecipeOwnedProject) => void;
}> = ({ projects, error, blocker, onDelete }) => {
  const titleId = React.useId();
  return (
    <section aria-labelledby={titleId}>
      <Stack hasGutter>
        <StackItem>
          <Title headingLevel="h2" size="xl" id={titleId}>Projects recipes created</Title>
        </StackItem>
        <StackItem>
          <Content component="p">
            Each is deleted with everything in it, only while it is still the one the recipe created. One that holds what this tool did not create is deleted only once you confirm it.
          </Content>
        </StackItem>
        {error && (
          <StackItem>
            <Alert variant="warning" isInline component="p" title="Cannot list the projects">{error}</Alert>
          </StackItem>
        )}
        {projects.length > 0 && (
          <StackItem>
            <Table aria-label="Projects recipes created" variant="compact">
              <Thead>
                <Tr>
                  <Th>Project</Th>
                  <Th>Recipe</Th>
                  <Th>Created</Th>
                  <Th screenReaderText="Actions" />
                </Tr>
              </Thead>
              <Tbody>
                {projects.map((p) => (
                  <Tr key={p.name}>
                    <Td dataLabel="Project">{p.name}</Td>
                    <Td dataLabel="Recipe">{p.recipe}</Td>
                    <Td dataLabel="Created">{p.createdAt ? <RelativeTime date={p.createdAt} /> : "Not recorded"}</Td>
                    <Td dataLabel="Actions" isActionCell>
                      <TooltipButton variant="secondary" isDanger size="sm" aria-label={`Delete project ${p.name}...`} disabledReason={blocker} onClick={() => onDelete(p)}>
                        Delete...
                      </TooltipButton>
                    </Td>
                  </Tr>
                ))}
              </Tbody>
            </Table>
          </StackItem>
        )}
      </Stack>
    </section>
  );
};
