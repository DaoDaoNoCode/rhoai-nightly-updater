import React from "react";
import { AlertActionLink } from "@patternfly/react-core";
import { Link, useNavigate } from "react-router-dom";

/**
 * A router Link to `to` for a PatternFly Button's `component` prop: the
 * button navigates inside the app (and opens in a new tab on middle-click).
 */
export const linkTo = (to: string) => (props: React.AnchorHTMLAttributes<HTMLAnchorElement>) => <Link {...props} to={to} />;

/** An alert's "Open Diagnostics" action. */
export const OpenDiagnosticsLink: React.FC = () => {
  const navigate = useNavigate();
  return <AlertActionLink onClick={() => navigate("/diagnostics")}>Open Diagnostics</AlertActionLink>;
};
