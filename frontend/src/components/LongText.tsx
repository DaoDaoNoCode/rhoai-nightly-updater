import React, { useState } from "react";
import { CodeBlock, CodeBlockCode, ExpandableSection } from "@patternfly/react-core";

/**
 * The first clause of a backend message, for a one-line summary: up to the
 * first line break, sentence end or ": " that starts a nested error.
 */
export function firstClause(text: string, max = 120): string {
  const firstLine = text.trim().split("\n")[0];
  const cut = firstLine.search(/(\.\s)|(:\s)|(;\s)/);
  let clause = cut > 0 ? firstLine.slice(0, cut) : firstLine.replace(/[.:;]$/, "");
  if (clause.length > max) clause = `${clause.slice(0, max - 1).trimEnd()}…`;
  return clause;
}

/**
 * Prose that may be long (operator and Kubernetes messages): shown in full
 * up to `lines` lines, with "Show more" when it is longer.
 */
export const TruncatedText: React.FC<{ children: React.ReactNode; lines?: number }> = ({ children, lines = 2 }) => {
  const [expanded, setExpanded] = useState(false);
  return (
    <ExpandableSection
      variant="truncate"
      truncateMaxLines={lines}
      isExpanded={expanded}
      onToggle={(_e, value) => setExpanded(value)}
      toggleTextCollapsed="Show more"
      toggleTextExpanded="Show less"
    >
      {children}
    </ExpandableSection>
  );
};

/**
 * Machine output (logs, raw errors, JSON) behind a "Show details" toggle,
 * in a code block that keeps line breaks and wraps long tokens.
 */
export const TechnicalDetails: React.FC<{ text: string | string[]; toggleText?: string }> = ({ text, toggleText = "Show details" }) => {
  const [expanded, setExpanded] = useState(false);
  const body = Array.isArray(text) ? text.join("\n") : text;
  if (!body.trim()) return null;
  return (
    <ExpandableSection
      toggleText={expanded ? "Hide details" : toggleText}
      isExpanded={expanded}
      onToggle={(_e, value) => setExpanded(value)}
    >
      <CodeBlock>
        <CodeBlockCode>{body}</CodeBlockCode>
      </CodeBlock>
    </ExpandableSection>
  );
};
