import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  CodeBlock,
  CodeBlockCode,
  Content,
  ExpandableSection,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  List,
  ListItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Radio,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from "@patternfly/react-core";
import { Table, Tbody, Td, Th, Thead, TreeRowWrapper, Tr } from "@patternfly/react-table";
import type { DSCChange, DSCManager, DSCNode, DSCPendingHardReset, DSCPlan, OperationResponse, UpdateStep } from "../../types";
import { finishDSCHardReset, getDSCManager, getDSCPlan, runDSCPlan, setDSCManagementState, streamDSCHardReset, toApiError } from "../../services/api";
import { ConfirmActionModal } from "../ConfirmActionModal";
import { StatusLabel, TagLabel, type StatusKind } from "../StatusLabel";
import { TooltipButton } from "../TooltipButton";
import { TechnicalDetails, TruncatedText } from "../LongText";
import { StepProgress, type StepState } from "../environment/StepProgress";
import { errorResult, outcomeTitle, outcomeVariant } from "../../outcomes";

/** A managementState as a status label. */
function stateKind(state?: string): StatusKind {
  if (state === "Managed") return "success";
  if (state === "Unmanaged") return "info";
  return "neutral";
}

/** What the operator reports for a node: its condition, else its status managementState. */
const ActualCell: React.FC<{ node: DSCNode }> = ({ node }) => {
  const c = node.condition;
  if (!c) return node.actual ? <StatusLabel status={stateKind(node.actual)}>{node.actual}</StatusLabel> : <span className="pf-v6-u-text-color-subtle">Not reported</span>;
  const label = c.status === "True" ? "Ready" : c.reason === "Removed" ? "Removed" : c.reason || "Not ready";
  const kind: StatusKind = c.status === "True" ? "success" : c.reason === "Removed" ? "neutral" : "warning";
  const status = <StatusLabel status={kind}>{label}</StatusLabel>;
  if (kind !== "warning" || !c.message) return status;
  return <>{status}<div className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle pf-v6-u-mt-xs"><TruncatedText>{c.message}</TruncatedText></div></>;
};

/** A change of a plan in words. */
export function changeText(ch: DSCChange): React.ReactNode {
  const from = ch.fromSet ? ch.from : "not set";
  const to = ch.toSet ? ch.to : "not set";
  const effect = ch.effect === "deploys" ? " (deploys it)" : ch.effect === "removes" ? " (removes it)" : "";
  if (from === to) {
    return <><code>{ch.path}</code> stays {from}{ch.effect === "removes" ? ", but deploys nothing: its parent is no longer Managed" : ch.effect === "deploys" ? " and is deployed again: its parent is Managed" : ""}</>;
  }
  return <><code>{ch.path}</code>: {from} → {to}{effect}</>;
}

/** The blocked removals and warnings of a plan. */
const PlanAlerts: React.FC<{ plan: DSCPlan }> = ({ plan }) => (
  <Stack hasGutter>
    {(plan.blocked?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="danger" isInline title="This change is not possible now">
          <Content component="p">These components must not be removed yet; nothing is changed while any is listed. Resolve the reasons and try again.</Content>
          <List>{plan.blocked!.map((b) => <ListItem key={b.component}><code>{b.component}</code>: {b.reasons.join(" ")}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
    {(plan.warnings?.length ?? 0) > 0 && (
      <StackItem>
        <Alert component="div" variant="warning" isInline title="Before you confirm">
          <List>{plan.warnings!.map((w) => <ListItem key={w}>{w}</ListItem>)}</List>
        </Alert>
      </StackItem>
    )}
  </Stack>
);

/** The tree of components and sub-components, with a state control per row. */
const ComponentTree: React.FC<{
  manager: DSCManager;
  disabledReason: string | null;
  pending: string | null;
  onSet: (node: DSCNode, to: string) => void;
}> = ({ manager, disabledReason, pending, onSet }) => {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const nodes = manager.nodes;
  const children = (path: string) => nodes.filter((n) => n.parent === path);
  const hidden = (n: DSCNode): boolean => {
    for (let p = n.parent; p; p = p.includes(".") ? p.slice(0, p.lastIndexOf(".")) : "") {
      if (collapsed[p]) return true;
    }
    return false;
  };
  const visible = nodes.filter((n) => !hidden(n));
  return (
    <Table isTreeTable aria-label="Components of the DataScienceCluster" variant="compact">
      <Thead>
        <Tr>
          <Th modifier="wrap">Component</Th>
          <Th modifier="wrap">In the DSC</Th>
          <Th modifier="wrap">Release default</Th>
          <Th modifier="wrap">Operator reports</Th>
        </Tr>
      </Thead>
      <Tbody>
        {visible.map((n, rowIndex) => {
          const siblings = n.parent ? children(n.parent) : nodes.filter((x) => x.depth === 0);
          const kids = children(n.path);
          const treeRow = {
            onCollapse: () => setCollapsed((c) => ({ ...c, [n.path]: !c[n.path] })),
            rowIndex,
            props: {
              isExpanded: !collapsed[n.path],
              "aria-level": n.depth + 1,
              "aria-posinset": siblings.indexOf(n) + 1,
              "aria-setsize": kids.length,
            },
          };
          const allowed = n.allowed ?? [];
          const rowReason = disabledReason ?? (allowed.length === 0 ? "The CRD lists no values for it, so this page does not change it." : null);
          return (
            <TreeRowWrapper key={n.path} row={{ props: treeRow.props }}>
              <Td dataLabel="Component" treeRow={treeRow}>
                <code>{n.name}</code>
              </Td>
              <Td dataLabel="In the DSC">
                {!n.toggle ? <span className="pf-v6-u-text-color-subtle">Group</span> : (
                  <Flex gap={{ default: "gapSm" }} alignItems={{ default: "alignItemsCenter" }}>
                    {/* The value in the DSC, and the control that changes it (the CRD's values only). */}
                    <FlexItem className="pf-v6-u-min-width" style={{ "--pf-v6-u-min-width--MinWidth": "16ch" } as React.CSSProperties}>
                      <FormSelect aria-label={`Set ${n.path}`} id={`dsc-set-${n.path}`} value={n.specSet ? n.spec : ""}
                        isDisabled={!!rowReason || pending !== null}
                        onChange={(_e, v) => { if (v && !(n.specSet && n.spec === v)) onSet(n, v); }}>
                        {!n.specSet && <FormSelectOption value="" label={`Not set (${n.schemaDefault || "Removed"})`} isDisabled />}
                        {allowed.map((v) => <FormSelectOption key={v} value={v} label={v} />)}
                      </FormSelect>
                    </FlexItem>
                    {pending === n.path && <FlexItem><Spinner size="sm" aria-label={`Changing ${n.path}`} /></FlexItem>}
                    {n.changed && <FlexItem><TagLabel color="blue">Changed from the default</TagLabel></FlexItem>}
                  </Flex>
                )}
              </Td>
              <Td dataLabel="Release default">
                {!n.toggle ? null : n.defaultSet ? n.default : <span className="pf-v6-u-text-color-subtle">{n.schemaDefault ? `Not set (CRD default ${n.schemaDefault})` : "Not set"}</span>}
              </Td>
              <Td dataLabel="Operator reports"><ActualCell node={n} /></Td>
            </TreeRowWrapper>
          );
        })}
      </Tbody>
    </Table>
  );
};

type HardStep = { id: string; title: string };
const HARD_RESET_STEPS: HardStep[] = [
  { id: "check", title: "Check the DataScienceCluster, the operator and every component" },
  { id: "delete", title: "Delete the DataScienceCluster" },
  { id: "wait_deleted", title: "Wait for the operator to remove every component" },
  { id: "create", title: "Create the release default" },
  { id: "wait_ready", title: "Wait for the components" },
];

/** Reset to the release default: soft (in place) or hard (delete and recreate). */
const ResetModal: React.FC<{
  name: string;
  disabledReason: string | null;
  onClose: () => void;
  onDone: (res: OperationResponse) => void;
}> = ({ name, disabledReason, onClose, onDone }) => {
  const titleId = React.useId();
  const [kind, setKind] = useState<"soft-reset" | "hard-reset">("soft-reset");
  const [plan, setPlan] = useState<DSCPlan | null>(null);
  const [planError, setPlanError] = useState("");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [steps, setSteps] = useState<Record<string, StepState>>({});
  const [result, setResult] = useState<OperationResponse | null>(null);
  const [yamlOpen, setYamlOpen] = useState(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const started = Object.keys(steps).length > 0;

  useEffect(() => {
    let active = true;
    setPlan(null);
    setPlanError("");
    getDSCPlan(kind).then((p) => { if (active) setPlan(p); }).catch((e) => { if (active) setPlanError(toApiError(e).message); });
    return () => { active = false; };
  }, [kind]);

  const blocked = !plan || !!plan.refusal || (plan.blocked?.length ?? 0) > 0;
  const confirm = async () => {
    if (!plan || blocked) return;
    setBusy(true);
    if (kind === "soft-reset") {
      let res: OperationResponse;
      try {
        res = await runDSCPlan("soft-reset", plan);
      } catch (e) {
        res = errorResult(e, "Could not reset the DataScienceCluster");
      }
      setBusy(false);
      onDone(res);
      onClose();
      return;
    }
    let final = "";
    controller.current = streamDSCHardReset(plan, typed,
      (step: UpdateStep) => {
        if (step.step === "operation_complete") { final = step.message; return; }
        setSteps((s) => ({ ...s, [step.step]: { status: step.status, message: step.message } }));
      },
      (success, error, apiError) => {
        const res: OperationResponse = { success, message: success ? final : error || final || "The hard reset failed.", logs: [], errorCode: apiError?.errorCode };
        setBusy(false);
        setResult(res);
        onDone(res);
      },
      () => {
        const res: OperationResponse = { success: false, message: "The connection to the server was lost. The hard reset continues on the server; this page shows the DataScienceCluster when it ends.", logs: [], errorCode: "detached" };
        setBusy(false);
        setResult(res);
        onDone(res);
      });
  };

  const removals = plan?.changes.filter((c) => c.effect === "removes") ?? [];
  const deploys = plan?.changes.filter((c) => c.effect === "deploys") ?? [];
  return (
    <Modal aria-labelledby={titleId} variant={ModalVariant.medium} isOpen onClose={() => !busy && onClose()}>
      <ModalHeader title="Reset to the release default" labelId={titleId} titleIconVariant={kind === "hard-reset" ? "warning" : undefined} />
      <ModalBody>
        <Stack hasGutter>
          {!started && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup role="radiogroup" isStack fieldId="dsc-reset-kind" label="How">
                  <Radio id="dsc-reset-soft" name="dsc-reset-kind" isChecked={kind === "soft-reset"} onChange={() => setKind("soft-reset")}
                    label="Soft reset (recommended)"
                    description={<>Changes <code>{name}</code> in place: only the components and settings that differ from the release default. Namespaces of workbenches and the AI hub are kept.</>} />
                  <Radio id="dsc-reset-hard" name="dsc-reset-kind" isChecked={kind === "hard-reset"} onChange={() => setKind("hard-reset")}
                    label="Hard reset"
                    description={<>Deletes <code>{name}</code> and creates the release default in its place. Use it when components are stuck and a soft reset does not help.</>} />
                </FormGroup>
              </Form>
            </StackItem>
          )}
          {planError && <StackItem><Alert component="p" variant="danger" isInline title="Could not work out the reset">{planError}</Alert></StackItem>}
          {!plan && !planError && <StackItem><Spinner size="md" aria-label="Working out the reset" /></StackItem>}
          {plan?.refusal && <StackItem><Alert component="p" variant="warning" isInline title="Not possible now">{plan.refusal}</Alert></StackItem>}
          {plan && !plan.refusal && !started && kind === "soft-reset" && (
            <StackItem>
              <Content component="p">This changes the shared cluster:</Content>
              <List>{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
            </StackItem>
          )}
          {plan && !plan.refusal && !started && kind === "hard-reset" && (
            <>
              <StackItem>
                <Alert component="div" variant="danger" isInline title="Every component is torn down, then comes back as the release default">
                  <List>
                    <ListItem>The operator removes every component ({(plan.tornDown ?? []).join(", ") || "none enabled"}), then the release default brings back what it enables. This takes minutes; RHOAI is unavailable meanwhile.</ListItem>
                    <ListItem>Your resources (notebooks, pipeline servers, models, model registries) are kept, but stop working until their components are back.</ListItem>
                    {(plan.stayRemoved?.length ?? 0) > 0 && (
                      <ListItem>Enabled now, but Removed in the release default, so they stay Removed: {plan.stayRemoved!.map((p, i) => <React.Fragment key={p}>{i > 0 && ", "}<code>{p}</code></React.Fragment>)}. Turn them on again afterwards if you need them.</ListItem>
                    )}
                    <ListItem>If the old DataScienceCluster is not gone within 10 minutes, nothing is created and the reset stops; the finalizer is never removed by hand. Once it is gone, this page offers to finish the reset with the same object.</ListItem>
                  </List>
                </Alert>
              </StackItem>
              {plan.changes.length > 0 && (
                <StackItem>
                  <Content component="p">Compared with now, the new DataScienceCluster differs in:</Content>
                  <List aria-label="Differences of the new DataScienceCluster">{plan.changes.map((c) => <ListItem key={c.path}>{changeText(c)}</ListItem>)}</List>
                </StackItem>
              )}
              <StackItem>
                <ExpandableSection toggleText="The DataScienceCluster it creates" isExpanded={yamlOpen} onToggle={(_e, v) => setYamlOpen(v)}>
                  <CodeBlock><CodeBlockCode>{plan.newObject}</CodeBlockCode></CodeBlock>
                </ExpandableSection>
              </StackItem>
            </>
          )}
          {plan && !started && <StackItem><PlanAlerts plan={plan} /></StackItem>}
          {plan && !plan.refusal && !started && kind === "hard-reset" && !blocked && (
            <StackItem>
              <Form onSubmit={(e) => e.preventDefault()}>
                <FormGroup label={<>Type <code>{name}</code> to confirm</>} fieldId="dsc-hard-reset-confirm" isRequired>
                  <TextInput id="dsc-hard-reset-confirm" value={typed} onChange={(_e, v) => setTyped(v)} autoComplete="off" />
                </FormGroup>
              </Form>
            </StackItem>
          )}
          {started && (
            <StackItem>
              <StepProgress idPrefix="dsc-hard-reset" aria-label="Hard reset progress" steps={HARD_RESET_STEPS} states={steps} running={busy}
                shownElsewhere={result && !result.success ? result.message : undefined} />
            </StackItem>
          )}
          {result && (
            <StackItem>
              <Alert component="p" isInline isLiveRegion variant={outcomeVariant(result)} title={outcomeTitle(result, "The hard reset stopped")}>{result.success ? undefined : result.message}</Alert>
            </StackItem>
          )}
          {kind === "soft-reset" && plan && !plan.refusal && (removals.length > 0 || deploys.length > 0) && (
            <StackItem>
              <Content component="p" className="pf-v6-u-text-color-subtle">
                {removals.length > 0 && <>{removals.length} {removals.length === 1 ? "component is" : "components are"} removed. </>}
                {deploys.length > 0 && <>{deploys.length} {deploys.length === 1 ? "component is" : "components are"} deployed.</>}
              </Content>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {result || started ? (
          <Button variant="primary" onClick={onClose} isDisabled={busy}>Close</Button>
        ) : (
          <>
            <TooltipButton variant={kind === "hard-reset" ? "danger" : "primary"} onClick={confirm} isLoading={busy}
              isDisabled={busy || blocked || !!disabledReason || (kind === "hard-reset" && typed !== name)} disabledReason={disabledReason}>
              {kind === "hard-reset" ? "Hard reset" : "Reset"}
            </TooltipButton>
            <Button variant="link" onClick={onClose} isDisabled={busy}>Cancel</Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};

/** A hard reset that deleted the DataScienceCluster and did not create its replacement, while none exists. */
const FinishHardReset: React.FC<{
  pending?: DSCPendingHardReset;
  error?: string;
  disabledReason: string | null;
  onDone: (res: OperationResponse) => void;
}> = ({ pending, error, disabledReason, onDone }) => {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [yamlOpen, setYamlOpen] = useState(false);
  const finish = async () => {
    if (!pending) return;
    setBusy(true);
    let res: OperationResponse;
    try {
      res = await finishDSCHardReset(pending.name, pending.token);
    } catch (e) {
      res = errorResult(e, "Could not finish the hard reset");
    }
    setBusy(false);
    setConfirming(false);
    onDone(res);
  };
  return (
    <PageSection>
      <Card>
        <CardHeader actions={pending ? {
          actions: (
            <TooltipButton variant="primary" onClick={() => setConfirming(true)} isDisabled={!!disabledReason || busy} disabledReason={disabledReason}>
              Finish the hard reset…
            </TooltipButton>
          ),
          hasNoOffset: false,
        } : undefined}>
          <CardTitle><Title headingLevel="h2" size="lg">A hard reset did not finish</Title></CardTitle>
        </CardHeader>
        <CardBody>
          <Stack hasGutter>
            {error && <StackItem><Alert component="p" variant="danger" isInline title="Could not read the recorded hard reset">{error}</Alert></StackItem>}
            {pending && (
              <>
                <StackItem>
                  <Content component="p">
                    A hard reset{pending.user ? <> by {pending.user}</> : null} at {pending.startedAt} deleted DataScienceCluster <code>{pending.name}</code> and did not create its replacement.
                    Finish it to create exactly the object it planned: the release default of operator {pending.operatorVersion || "(unknown)"}, with the old name and namespaces.
                    Create DataScienceCluster would create the release default without the old name and namespaces instead.
                  </Content>
                </StackItem>
                <StackItem>
                  <ExpandableSection toggleText="The DataScienceCluster it creates" isExpanded={yamlOpen} onToggle={(_e, v) => setYamlOpen(v)}>
                    <CodeBlock><CodeBlockCode>{pending.newObject}</CodeBlockCode></CodeBlock>
                  </ExpandableSection>
                </StackItem>
              </>
            )}
          </Stack>
        </CardBody>
      </Card>
      {pending && confirming && (
        <ConfirmActionModal
          isOpen
          title="Finish the hard reset?"
          changes={[<>Creates DataScienceCluster <code>{pending.name}</code> ({pending.apiVersion}) exactly as the hard reset planned it.</>]}
          changesOpen
          confirmLabel={`Create ${pending.name}`}
          isLoading={busy}
          confirmDisabled={busy || !!disabledReason}
          onConfirm={finish}
          onCancel={() => !busy && setConfirming(false)}
        >
          <Content component="p" className="pf-v6-u-text-color-subtle">
            The server checks again that no DataScienceCluster exists and that the operator is the one the reset planned for, then runs a dry run first.
          </Content>
        </ConfirmActionModal>
      )}
    </PageSection>
  );
};

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
              <Content component="p" className="pf-v6-u-text-color-subtle">
                From the installed DataScienceCluster CRD ({manager.apiVersion?.split("/")[1]}); the release default is the example the installed operator {manager.operatorVersion} ships. A change here patches only the field you change.
              </Content>
            </StackItem>
            {manager.readOnlyReason && <StackItem><Alert component="p" variant="info" isInline title="This page does not change the DataScienceCluster now">{manager.readOnlyReason}</Alert></StackItem>}
            {manager.defaultsError && <StackItem><Alert component="p" variant="warning" isInline title="No release default to compare with">{manager.defaultsError}</Alert></StackItem>}
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
            {manager.mismatches.length > 0 && (
              <StackItem>
                <Card isPlain isCompact>
                  <CardHeader actions={fixable.length > 0 ? {
                    actions: (
                      <TooltipButton variant="primary" isDisabled={!!disabledReason || running !== null} disabledReason={disabledReason}
                        onClick={() => openPlan({ title: "Fix the differences from the release default?", confirmLabel: "Fix" })}>
                        Fix
                      </TooltipButton>
                    ),
                  } : undefined}>
                    <CardTitle><Title headingLevel="h3" size="md">Differences from the release default</Title></CardTitle>
                  </CardHeader>
                  <CardBody>
                    <List aria-label="Differences from the release default">
                      {manager.mismatches.map((m) => (
                        <ListItem key={m.kind + m.path}>
                          <code>{m.path}</code>: {m.message}
                        </ListItem>
                      ))}
                    </List>
                  </CardBody>
                </Card>
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
