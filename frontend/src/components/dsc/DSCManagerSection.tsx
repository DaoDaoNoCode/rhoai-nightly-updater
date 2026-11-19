import React, { useCallback, useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Content,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  Title,
} from "@patternfly/react-core";
import type { DSCManager, DSCNode, DSCPlan, OperationResponse } from "../../types";
import { getDSCManager, getDSCPlan, runDSCPlan, setDSCManagementState, toApiError } from "../../services/api";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { TooltipButton } from "../TooltipButton";
import { TechnicalDetails } from "../LongText";
import { errorResult, outcomeTitle, outcomeVariant } from "../../outcomes";
import { PlanAlerts, changeText } from "./planText";
import { ComponentTree } from "./ComponentTree";
import { ComparisonSummary } from "./ComparisonSummary";
import { FinishHardReset, ResetModal } from "./ResetModal";

/** A change waiting for confirmation: a toggle or a fix. */
interface PendingChange {
  title: string;
  confirmLabel: string;
  toggle?: { node: DSCNode; to: string; withParent: boolean };
  plan: DSCPlan | null;
  error?: string;
}

/**
 * The DataScienceCluster's components and sub-components from the installed
 * CRD, compared with the release default, with a state control per row,
 * the differences that are real problems (Fix), and Reset to the release
 * default. Every change is confirmed with its exact change list.
 */
export const DSCManagerSection: React.FC<{
  /** Reloads the tree when it changes (the page's last refresh). */
  refreshKey: unknown;
  /** Why changes are off for this user or profile, or null. */
  mutateReason: string | null;
  onResult: (res: OperationResponse) => void;
  onChanged: () => void;
}> = ({ refreshKey, mutateReason, onResult, onChanged }) => {
  const [manager, setManager] = useState<DSCManager | null>(null);
  const [loadError, setLoadError] = useState("");
  const [pending, setPending] = useState<PendingChange | null>(null);
  const [running, setRunning] = useState<string | null>(null);
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [resetOpen, setResetOpen] = useState(false);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let active = true;
    getDSCManager().then((m) => { if (active) { setManager(m); setLoadError(""); } })
      .catch((e) => { if (active) setLoadError(toApiError(e).message); });
    return () => { active = false; };
  }, [refreshKey, reload]);

  const changed = useCallback((res: OperationResponse) => {
    setResult(res);
    onResult(res);
    onChanged();
    setReload((n) => n + 1);
  }, [onChanged, onResult]);

  const openPlan = (p: Omit<PendingChange, "plan">) => {
    setPending({ ...p, plan: null });
    const req = p.toggle ? getDSCPlan("toggle", { path: p.toggle.node.path, to: p.toggle.to, withParent: p.toggle.withParent }) : getDSCPlan("fix");
    req.then((plan) => setPending((cur) => (cur && cur.title === p.title && cur.toggle?.withParent === p.toggle?.withParent ? { ...cur, plan } : cur)))
      .catch((e) => setPending((cur) => (cur ? { ...cur, error: toApiError(e).message } : cur)));
  };

  if (!manager && !loadError) return null;
  if (loadError && !manager) {
    return (
      <PageSection>
        <Alert component="p" variant="warning" isInline title="Could not load the component tree">{loadError}</Alert>
      </PageSection>
    );
  }
  if (!manager) return null;
  if (!manager.exists) {
    if (!manager.pendingHardReset && !manager.pendingHardResetError) return null;
    return <FinishHardReset pending={manager.pendingHardReset} error={manager.pendingHardResetError} disabledReason={mutateReason} onDone={changed} />;
  }

  const disabledReason = mutateReason ?? (manager.readOnlyReason || null);
  const fixable = manager.mismatches.filter((m) => m.fixable);

  const confirmPending = async () => {
    if (!pending?.plan || !manager.name) return;
    const p = pending;
    setPending(null);
    setResult(null);
    let res: OperationResponse;
    try {
      if (p.toggle) {
        setRunning(p.toggle.node.path);
        res = await setDSCManagementState({ name: manager.name, path: p.toggle.node.path, from: p.toggle.node.specSet ? p.toggle.node.spec ?? "" : "", to: p.toggle.to, withParent: p.toggle.withParent, uid: p.plan!.uid ?? "", token: p.plan!.token ?? "" });
      } else {
        setRunning("fix");
        res = await runDSCPlan("fix", p.plan!);
      }
    } catch (e) {
      res = errorResult(e, "Could not change the DataScienceCluster");
    }
    setRunning(null);
    changed(res);
  };

  const plan = pending?.plan;
  const planBlocked = !plan || !!plan.refusal || (plan.blocked?.length ?? 0) > 0;
  return (
    <PageSection>
      <Card>
        <CardHeader actions={{
          actions: (
            <TooltipButton variant="secondary" onClick={() => setResetOpen(true)} isDisabled={!!disabledReason || !!manager.defaultsError || running !== null} disabledReason={disabledReason ?? (manager.defaultsError || null)}>
              Reset to the release default…
            </TooltipButton>
          ),
          hasNoOffset: false,
        }}>
          <CardTitle><Title headingLevel="h2" size="lg">Components and sub-components</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            <StackItem>
              <ComparisonSummary manager={manager} fix={fixable.length > 0 ? (
                <TooltipButton variant="link" isInline isDisabled={!!disabledReason || running !== null} disabledReason={disabledReason}
                  onClick={() => openPlan({ title: "Fix the differences from the release default?", confirmLabel: "Fix" })}>
                  Fix
                </TooltipButton>
              ) : null} />
            </StackItem>
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">
                From the installed DataScienceCluster CRD ({manager.apiVersion?.split("/")[1]}). A change here patches only the field you change.
              </Content>
            </StackItem>
            {manager.readOnlyReason && <StackItem><Alert component="p" variant="info" isInline title="This page does not change the DataScienceCluster now">{manager.readOnlyReason}</Alert></StackItem>}
            {(manager.upgradeGates?.length ?? 0) > 0 && (
              <StackItem>
                <Alert component="div" variant="warning" isInline title="An upgrade gate holds provisioning">
                  <Content component="p">The operator waits for an admin acknowledgement in ConfigMap <code>odh-upgrade-acks</code>. Diagnostics shows the command.</Content>
                  <TechnicalDetails text={manager.upgradeGates!} />
                </Alert>
              </StackItem>
            )}
            {result && (
              <StackItem>
                <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "Could not change the DataScienceCluster")}
                  actionClose={<AlertActionCloseButton onClose={() => setResult(null)} />}>
                  {result.success ? undefined : result.message}
                </Alert>
              </StackItem>
            )}
          </Stack>
        </CardBody>
        <CardBody className="pf-v6-u-px-0">
          <ComponentTree manager={manager} disabledReason={disabledReason} pending={running}
            onSet={(node, to) => openPlan({ title: `Set ${node.path} to ${to}?`, confirmLabel: `Set to ${to}`, toggle: { node, to, withParent: false } })} />
        </CardBody>
      </Card>

      {pending && (
        <ConfirmActionModal
          isOpen
          title={pending.title}
          changes={plan && !plan.refusal ? plan.changes.map((c) => changeText(c)) : []}
          summary={!plan ? (pending.error ? <Content component="p">Nothing is changed.</Content> : <Spinner size="md" aria-label="Working out the change" />) : plan.refusal ? <Content component="p">Nothing would change.</Content> : undefined}
          changesOpen
          confirmLabel={pending.confirmLabel}
          confirmVariant={plan?.changes.some((c) => c.effect === "removes") ? "danger" : "primary"}
          confirmDisabled={planBlocked || !!disabledReason}
          onConfirm={confirmPending}
          onCancel={() => setPending(null)}
        >
          <Stack hasGutter>
            {pending.error && <StackItem><Alert component="p" variant="danger" isInline title="Could not work out the change">{pending.error}</Alert></StackItem>}
            {plan?.refusal && (
              <StackItem>
                <Alert component="div" variant="warning" isInline title={plan.code === "parent_removed" ? "Its parent is not Managed" : "Not possible now"}>
                  <Content component="p">{plan.refusal}</Content>
                  {plan.code === "parent_removed" && pending.toggle && plan.parentPath && (
                    <Button variant="link" isInline onClick={() => openPlan({ ...pending, title: `Set ${pending.toggle!.node.path} and ${plan.parentPath} to Managed?`, toggle: { ...pending.toggle!, withParent: true } })}>
                      Also set {plan.parentPath} to Managed
                    </Button>
                  )}
                </Alert>
              </StackItem>
            )}
            {plan && <StackItem><PlanAlerts plan={plan} /></StackItem>}
            {plan && !plan.refusal && (
              <StackItem>
                <Content component="p" className="pf-v6-u-text-color-subtle">
                  The server checks the DataScienceCluster again, runs a dry run, and changes nothing if any of these fields changed since you looked.
                </Content>
              </StackItem>
            )}
          </Stack>
        </ConfirmActionModal>
      )}

      {resetOpen && manager.name && (
        <ResetModal name={manager.name} disabledReason={disabledReason} onClose={() => setResetOpen(false)} onDone={changed} />
      )}
    </PageSection>
  );
};
