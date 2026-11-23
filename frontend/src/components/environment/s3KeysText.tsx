import React from "react";

/** Parses a KeepOldKey value ("ns/name (reason)") into the server label and its rationale. */
export const keptEntry = (entry: string) => {
  const at = entry.indexOf(" (");
  return at < 0 ? { name: entry, reason: "" } : { name: entry.slice(0, at), reason: entry.slice(at + 2, -1) };
};

/** Displays a value in monospace so hyphenated identifiers do not wrap mid-name. */
export const Id: React.FC<{ children: React.ReactNode }> = ({ children }) => <code style={{ whiteSpace: "nowrap" }}>{children}</code>;

/** Renders one retained server: name as inline code, optional reason in a non-wrapping span. */
export const KeptServer: React.FC<{ entry: string }> = ({ entry }) => {
  const { name, reason } = keptEntry(entry);
  return <><Id>{name}</Id>{reason && <> <span style={{ whiteSpace: "nowrap" }}>({reason})</span></>}</>;
};

/** Serializes React nodes into natural language with comma separators and a final "and". */
export function joinAnd(nodes: React.ReactNode[]): React.ReactNode {
  return nodes.map((n, i) => (
    <React.Fragment key={i}>{i === 0 ? "" : i === nodes.length - 1 ? " and " : ", "}{n}</React.Fragment>
  ));
}

// Patterns that denote names inside operation messages: pipeline servers ("project/name"),
// SeaweedFS users, and Secret objects this app creates or references.
const IDENTIFIER = /([a-z0-9](?:[-a-z0-9]*[a-z0-9])?\/[a-z0-9](?:[-a-z0-9]*[a-z0-9])?|rnu-dspa-[a-z0-9.-]*[a-z0-9]|rhoai-nightly-updater-s3|nightly-dspa-s3|seaweedfs-s3-credentials)/g;

/** Converts plain backend strings into React, highlighting known identifiers as code. */
export function withIds(text: string): React.ReactNode {
  const parts = text.split(IDENTIFIER);
  return parts.map((p, i) => (i % 2 === 1 ? <Id key={i}>{p}</Id> : <React.Fragment key={i}>{p}</React.Fragment>));
}