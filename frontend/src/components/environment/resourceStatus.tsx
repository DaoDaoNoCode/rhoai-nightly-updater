import React from "react";
import { Spinner } from "@patternfly/react-core";
import LockIcon from "@patternfly/react-icons/dist/esm/icons/lock-icon";
import type { ResourceState } from "../../types";
import { StatusLabel, TagLabel } from "../StatusLabel";
import { isTerminating, terminalReason } from "./resourceRow";

/**
 * The Environment page's status vocabulary: one health status per
 * resource, the same words on every card, goal step and drawer. Who
 * created a resource is not a health: a server someone else made can be
 * Ready. Ownership is shown apart (NotManagedLabel) and only decides
 * whether this tool may change or delete it.
 * - not-set-up (grey): nothing of it exists.
 * - setting-up (blue, spinner): it is starting, or this page runs its setup.
 * - deleting (blue, spinner): it is being deleted.
 * - ready (green): it works.
 * - needs-attention (orange): it needs an action the tool offers (Repair,
 *   Migrate storage); the drawer says why.
 * - error (red): it cannot start without a change; the drawer says why.
 * - unknown (grey): its health is not reported.
 */
export type EnvStatus = "not-set-up" | "setting-up" | "deleting" | "ready" | "needs-attention" | "error" | "unknown";

export interface EnvStatusOptions {
  /** An action from this page is changing it now. */
  busy?: "setting-up" | "deleting" | null;
  /** It needs the user's action although its state reads fine (storage of an earlier version to migrate). */
  attention?: boolean;
}

/** The status of one resource. */
export function envStatus(state: ResourceState | undefined, { busy, attention }: EnvStatusOptions = {}): EnvStatus {
  if (busy) return busy;
  if (!state) return "not-set-up";
  if (isTerminating(state)) return "deleting";
  if (attention) return "needs-attention";
  if (state.ready) return state.repairNeeded || state.warning ? "needs-attention" : "ready";
  if (!state.deployed) return "not-set-up";
  if (terminalReason(state)) return state.repairNeeded && state.managedByTool !== false ? "needs-attention" : "error";
  return "setting-up";
}

const TEXT: Record<EnvStatus, string> = {
  "not-set-up": "Not set up",
  "setting-up": "Setting up",
  deleting: "Deleting",
  ready: "Ready",
  "needs-attention": "Needs attention",
  error: "Error",
  unknown: "Unknown",
};

export const envStatusText = (status: EnvStatus) => TEXT[status];

/** The one status label of a resource. */
export const EnvStatusLabel: React.FC<{ status: EnvStatus }> = ({ status }) => {
  switch (status) {
    case "not-set-up":
      return <StatusLabel status="neutral">{TEXT[status]}</StatusLabel>;
    case "setting-up":
    case "deleting":
      // The label's text names the state; the spinner only shows it moves.
      return <StatusLabel status="progress" icon={<Spinner isInline aria-hidden />}>{TEXT[status]}</StatusLabel>;
    case "ready":
      return <StatusLabel status="success">{TEXT[status]}</StatusLabel>;
    case "needs-attention":
      return <StatusLabel status="warning">{TEXT[status]}</StatusLabel>;
    case "error":
      return <StatusLabel status="danger">{TEXT[status]}</StatusLabel>;
    case "unknown":
      return <StatusLabel status="neutral">{TEXT[status]}</StatusLabel>;
  }
};

/**
 * Something with the tool's name exists but this tool did not create it
 * (the backend also says managedByTool false when nothing exists: that is
 * not shown).
 */
export function notManaged(state: ResourceState | undefined): boolean {
  return state?.managedByTool === false && (state.deployed || isTerminating(state) || !!state.setupBlockedReason);
}

/** Ownership, beside the status: an outline label, not a status color. */
export const NotManagedLabel: React.FC = () => <TagLabel icon={<LockIcon />}>Not managed by this tool</TagLabel>;

/** A resource that can be used: ready, whoever created it. */
export function usable(state: ResourceState | undefined): boolean {
  return !!state?.ready && !isTerminating(state);
}
