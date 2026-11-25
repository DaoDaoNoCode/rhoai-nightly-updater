import React from "react";
import { Button } from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { GITOPS_DOC_URL } from "../constants";

/** A link to a section of the published GitOps guide (docs/GITOPS.md), opening in a new tab. */
export const GitOpsDocLink: React.FC<{ anchor?: string; children: React.ReactNode }> = ({ anchor, children }) => (
  <Button variant="link" isInline component="a" href={`${GITOPS_DOC_URL}${anchor ? `#${anchor}` : ""}`} target="_blank" rel="noopener noreferrer"
    icon={<ExternalLinkAltIcon />} iconPosition="end">
    {children}
  </Button>
);
