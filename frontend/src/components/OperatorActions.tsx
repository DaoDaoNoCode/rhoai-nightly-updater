import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  AlertGroup,
  Button,
  Checkbox,
  Content,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from "@patternfly/react-core";
import { streamRefresh, streamReinstall, streamUpdate, trackFeature, type OperatorOperationOptions } from "../services/api";
import type { DashboardOverride } from "../types";
import { useOperation } from "../state/AppState";
import { useDashboardOverride } from "../state/AppInfo";
import { formatRelativeTime } from "../utils";
import { describeOutcomeError } from "../errors";
import { sentence } from "../build";

export type ReinstallTargetType = "stable" | "nightly" | "custom";

/** An operator operation the Status page can start. */
export type OperatorRequest =
  | { kind: "update"; image: string }
  | { kind: "refresh"; csvName?: string }
  | { kind: "reinstall"; targetType: ReinstallTargetType; image?: string; channel?: string; detail: string };

const ACTION_NAMES: Record<OperatorRequest["kind"], string> = {
  update: "update",
  refresh: "re-deploy",
  reinstall: "reinstall",
};

/** "PR #222 (RHOAI build) by alice, 2h ago" */
export function describeDashboardSession(o: DashboardOverride | null): string {
  if (!o) return "a Dashboard Dev session";
  if (!o.sessionRecorded) return "dashboard-operator, scaled down outside this tool";
  const what = o.mode === "pr" && o.prNumber ? `PR #${o.prNumber}` : o.mode === "main" ? "the main branch" : "a dashboard build";
  const by = o.startedBy ? ` by ${o.startedBy}` : "";
  const when = o.startedAt ? `, ${formatRelativeTime(o.startedAt)}` : "";
  return `${what}${by}${when}`;
}

/**
 * What reverting the Dashboard Dev session does (B4 RevertDashboardDevForOperation:
 * resume dashboard-operator, which puts the release images back).
 */
export const REVERT_DASHBOARD_EXPLANATION =
  "Reverting resumes dashboard-operator, which puts the release dashboard images back; the Dashboard Dev deployment ends. You can deploy it again afterwards.";

interface PendingFollowUp {
  request: OperatorRequest;
  options: OperatorOperationOptions;
  message: string;
}

/**
 * Starts Update, Re-deploy and Reinstall, and handles the backend's two
 * refusals that need the user's confirmation (nothing changed on the
 * cluster in either case):
 * - dashboard_dev_active: offer to revert the Dashboard Dev session and
 *   continue (revertDashboardDev), or cancel.
 * - downgrade_requires_confirmation (Reinstall): explain and require an
 *   explicit confirmation before sending allowDowngrade.
 */
export function useOperatorActions(): {
  runOperator: (request: OperatorRequest, options?: OperatorOperationOptions) => boolean;
  followUps: React.ReactNode;
} {
  const operation = useOperation();
  const { override } = useDashboardOverride();
  const lastRef = useRef<{ runId: number; request: OperatorRequest; options: OperatorOperationOptions } | null>(null);
  const [dashboardConflict, setDashboardConflict] = useState<PendingFollowUp | null>(null);
  const [downgrade, setDowngrade] = useState<PendingFollowUp | null>(null);
  const [downgradeAck, setDowngradeAck] = useState(false);
  const [busyNotice, setBusyNotice] = useState<string | null>(null);

  const { start } = operation;
  const runOperator = useCallback((request: OperatorRequest, options: OperatorOperationOptions = {}): boolean => {
    let id: number | null;
    switch (request.kind) {
      case "update":
        trackFeature("update");
        id = start("update", (h) => streamUpdate(request.image, h.onStep, h.onDone, h.onDetach, options), request.image);
        break;
      case "refresh":
        trackFeature("refresh_operator");
        id = start("refresh", (h) => streamRefresh(h.onStep, h.onDone, h.onDetach, options), request.csvName);
        break;
      case "reinstall": {
        const { targetType, image, channel } = request;
        trackFeature(targetType === "stable" ? "reinstall_stable" : targetType === "custom" ? "reinstall_custom" : "reinstall_nightly");
        id = start(
          targetType === "stable" ? "reinstall_stable" : "reinstall_nightly",
          (h) => streamReinstall(targetType, image, channel, h.onStep, h.onDone, h.onDetach, options),
          request.detail,
        );
        break;
      }
    }
    if (id === null) return false;
    lastRef.current = { runId: id, request, options };
    return true;
  }, [start]);

  const run = operation.run;
  const outcome = run?.outcome;
  useEffect(() => {
    const last = lastRef.current;
    if (!run || !last || run.id !== last.runId || outcome?.status !== "failed") return;
    lastRef.current = null;
    if (outcome.errorCode === "cluster_busy") {
      // The progress area switches to the other operation, so say why this one did not run.
      setBusyNotice(describeOutcomeError(outcome, "Cluster busy").body);
    } else if (outcome.errorCode === "dashboard_dev_active" && !last.options.revertDashboardDev) {
      setDashboardConflict({ request: last.request, options: last.options, message: outcome.message });
    } else if (outcome.errorCode === "downgrade_requires_confirmation" && !last.options.allowDowngrade && last.request.kind === "reinstall") {
      setDowngradeAck(false);
      setDowngrade({ request: last.request, options: last.options, message: outcome.message });
    }
  }, [run, outcome]);

  const action = dashboardConflict ? ACTION_NAMES[dashboardConflict.request.kind] : "";
  const followUps = (
    <>
      <AlertGroup isToast isLiveRegion>
        {busyNotice && (
          <Alert
            variant="warning"
            title="Cluster busy: your request did not run"
            timeout={10_000}
            onTimeout={() => setBusyNotice(null)}
            actionClose={<AlertActionCloseButton onClose={() => setBusyNotice(null)} />}
          >
            {sentence(busyNotice)} Nothing was changed; its progress is shown on this page.
          </Alert>
        )}
      </AlertGroup>
      <Modal
        variant={ModalVariant.small}
        isOpen={!!dashboardConflict}
        onClose={() => setDashboardConflict(null)}
        aria-labelledby="dashboard-dev-conflict-title"
        aria-describedby="dashboard-dev-conflict-body"
      >
        <ModalHeader title={`Revert Dashboard Dev before the ${action}?`} labelId="dashboard-dev-conflict-title" titleIconVariant="warning" />
        <ModalBody id="dashboard-dev-conflict-body">
          <Stack hasGutter>
            <StackItem>
              <Content component="p">
                A Dashboard Dev session is active ({describeDashboardSession(override)}), so dashboard-operator is paused.
                The {action} would keep the dashboard paused under the new operator, so it did not start and nothing was changed.
              </Content>
            </StackItem>
            <StackItem>
              <Content component="p">{REVERT_DASHBOARD_EXPLANATION}</Content>
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="primary"
            onClick={() => {
              const pending = dashboardConflict;
              setDashboardConflict(null);
              if (pending) runOperator(pending.request, { ...pending.options, revertDashboardDev: true });
            }}
          >
            Revert Dashboard Dev and continue
          </Button>
          <Button variant="link" onClick={() => setDashboardConflict(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>

      <Modal
        variant={ModalVariant.small}
        isOpen={!!downgrade}
        onClose={() => setDowngrade(null)}
        aria-labelledby="downgrade-confirm-title"
        aria-describedby="downgrade-confirm-body"
      >
        <ModalHeader title="Install an older operator version?" labelId="downgrade-confirm-title" titleIconVariant="warning" />
        <ModalBody id="downgrade-confirm-body">
          <Stack hasGutter>
            <StackItem>
              <Alert variant="warning" isInline isPlain component="p" title="The reinstall did not start; nothing was changed." />
            </StackItem>
            <StackItem>
              <Content component="p">{downgrade?.message}</Content>
            </StackItem>
            <StackItem>
              <Checkbox
                id="downgrade-ack-followup"
                isChecked={downgradeAck}
                onChange={(_e, checked) => setDowngradeAck(checked)}
                label="I understand: OLM replaces the CRDs of the older bundle with their older versions, and the older operator may refuse to manage resources the newer version created."
              />
            </StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button
            variant="danger"
            isDisabled={!downgradeAck}
            onClick={() => {
              const pending = downgrade;
              setDowngrade(null);
              if (pending) runOperator(pending.request, { ...pending.options, allowDowngrade: true });
            }}
          >
            Reinstall the older version
          </Button>
          <Button variant="link" onClick={() => setDowngrade(null)}>Cancel</Button>
        </ModalFooter>
      </Modal>
    </>
  );

  return { runOperator, followUps };
}
