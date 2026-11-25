import React from "react";
import { Button } from "@patternfly/react-core";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import { docsURL } from "../constants";
import { useVersion } from "../state/AppInfo";

/** A link to a section of a published doc of this project (docs/GITOPS.md...), of the running release, in a new tab. */
export const DocLink: React.FC<{ path: string; anchor?: string; children: React.ReactNode }> = ({ path, anchor, children }) => {
  const version = useVersion()?.version;
  return (
    <Button variant="link" isInline component="a" href={docsURL(path, anchor, version)} target="_blank" rel="noopener noreferrer"
      icon={<ExternalLinkAltIcon />} iconPosition="end">
      {children}
    </Button>
  );
};
